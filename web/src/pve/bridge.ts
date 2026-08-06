export interface PveBridgeRequest {
  source: 'pvn-ui';
  type: 'pvn:pve:request';
  version: 1;
  nonce: string;
  id: string;
  method: 'GET' | 'PUT';
  path: string;
  params?: Record<string, unknown>;
}

interface PveBridgeResponse {
  source: 'pvn-loader';
  type: 'pvn:pve:response';
  version: 1;
  nonce: string;
  id: string;
  ok: boolean;
  data?: unknown;
  error?: { message: string; status?: number };
}

interface PendingRequest {
  resolve: (value: unknown) => void;
  reject: (reason: Error) => void;
  timer: number;
}

export interface PveBridgeBootstrap {
  nonce: string;
  origin: string;
}

export interface PveNicUpdate {
  digest: string;
  nic: `net${number}`;
  macAddress: string;
  linkDown: boolean;
}

export interface PveQemuStatus {
  status?: string;
  qmpstatus?: string;
  [key: string]: unknown;
}

export interface PveQemuVM {
  vmid: number;
  name?: string;
  node: string;
}

export function normalizeQemuVMs(value: unknown): PveQemuVM[] {
  if (!Array.isArray(value)) return [];
  return value.flatMap((item) => {
    if (!item || typeof item !== 'object') return [];
    const resource = item as Record<string, unknown>;
    if (resource.type !== 'qemu' || !Number.isSafeInteger(resource.vmid) || Number(resource.vmid) <= 0) return [];
    if (typeof resource.node !== 'string' || !/^[A-Za-z0-9._-]+$/.test(resource.node)) return [];
    const name = typeof resource.name === 'string' ? resource.name.trim() : '';
    return [{ vmid: Number(resource.vmid), node: resource.node, ...(name ? { name } : {}) }];
  });
}

export function readPveBridgeBootstrap(
  locationValue: Pick<Location, 'search'> = window.location,
  referrer = document.referrer,
): PveBridgeBootstrap | null {
  const params = new URLSearchParams(locationValue.search);
  const nonce = params.get('pveBridgeNonce') || '';
  const origin = params.get('pveOrigin') || '';
  if (!/^[A-Za-z0-9_-]{32,128}$/.test(nonce)) return null;
  let parsedOrigin: string;
  let referrerOrigin: string;
  try {
    parsedOrigin = new URL(origin).origin;
    referrerOrigin = new URL(referrer).origin;
  } catch {
    return null;
  }
  if (parsedOrigin !== origin || parsedOrigin !== referrerOrigin || !parsedOrigin.startsWith('https://')) {
    return null;
  }
  return { nonce, origin: parsedOrigin };
}

export class PveBridge {
  private readonly bootstrap: PveBridgeBootstrap | null;
  private readonly pending = new Map<string, PendingRequest>();
  private readonly onMessageBound: (event: MessageEvent) => void;

  constructor(bootstrap = readPveBridgeBootstrap()) {
    this.bootstrap = bootstrap;
    this.onMessageBound = (event) => this.onMessage(event);
    if (bootstrap && window.parent !== window) window.addEventListener('message', this.onMessageBound);
  }

  get available(): boolean {
    return this.bootstrap !== null && window.parent !== window;
  }

  get parentOrigin(): string | null {
    return this.bootstrap?.origin ?? null;
  }

  getQemuConfig(node: string, vmid: number): Promise<Record<string, unknown>> {
    return this.request('GET', this.qemuPath(node, vmid)) as Promise<Record<string, unknown>>;
  }

  getQemuStatus(node: string, vmid: number): Promise<PveQemuStatus> {
    return this.request('GET', this.qemuStatusPath(node, vmid)) as Promise<PveQemuStatus>;
  }

  async listQemuVMs(): Promise<PveQemuVM[]> {
    return normalizeQemuVMs(await this.request('GET', '/cluster/resources', { type: 'vm' }));
  }

  setQemuNic(node: string, vmid: number, update: PveNicUpdate): Promise<unknown> {
    this.assertDigestAndNic(update.digest, update.nic);
    if (!/^[A-Fa-f0-9]{2}(?::[A-Fa-f0-9]{2}){5}$/.test(update.macAddress)) {
      return Promise.reject(new Error('Invalid VM NIC MAC address'));
    }
    return this.request('PUT', this.qemuPath(node, vmid), {
      digest: update.digest,
      [update.nic]: `virtio=${update.macAddress},bridge=br-int,firewall=0,link_down=${update.linkDown ? 1 : 0}`,
    });
  }

  deleteQemuNic(node: string, vmid: number, digest: string, nic: `net${number}`): Promise<unknown> {
    this.assertDigestAndNic(digest, nic);
    return this.request('PUT', this.qemuPath(node, vmid), { digest, delete: nic });
  }

  destroy(): void {
    window.removeEventListener('message', this.onMessageBound);
    for (const pending of this.pending.values()) {
      window.clearTimeout(pending.timer);
      pending.reject(new Error('PVE bridge was closed'));
    }
    this.pending.clear();
  }

  private qemuPath(node: string, vmid: number): string {
    this.assertNodeAndVM(node, vmid);
    return `/nodes/${encodeURIComponent(node)}/qemu/${vmid}/config`;
  }

  private qemuStatusPath(node: string, vmid: number): string {
    this.assertNodeAndVM(node, vmid);
    return `/nodes/${encodeURIComponent(node)}/qemu/${vmid}/status/current`;
  }

  private assertNodeAndVM(node: string, vmid: number): void {
    if (!/^[A-Za-z0-9._-]+$/.test(node) || !Number.isSafeInteger(vmid) || vmid <= 0) {
      throw new Error('Invalid PVE node or VM ID');
    }
  }

  private assertDigestAndNic(digest: string, nic: string): void {
    if (!/^[A-Fa-f0-9]{40,128}$/.test(digest) || !/^net[0-9]+$/.test(nic)) {
      throw new Error('Invalid PVE config digest or NIC slot');
    }
  }

  private request(method: 'GET' | 'PUT', path: string, params?: Record<string, unknown>): Promise<unknown> {
    if (!this.bootstrap || window.parent === window) {
      return Promise.reject(new Error('Open PVN from the Proxmox Datacenter menu to manage VM NICs'));
    }
    const id = crypto.randomUUID();
    const message: PveBridgeRequest = {
      source: 'pvn-ui',
      type: 'pvn:pve:request',
      version: 1,
      nonce: this.bootstrap.nonce,
      id,
      method,
      path,
      ...(params ? { params } : {}),
    };
    return new Promise((resolve, reject) => {
      const timer = window.setTimeout(() => {
        this.pending.delete(id);
        reject(new Error('PVE did not answer the VM configuration request'));
      }, 15_000);
      this.pending.set(id, { resolve, reject, timer });
      window.parent.postMessage(message, this.bootstrap!.origin);
    });
  }

  private onMessage(event: MessageEvent): void {
    if (!this.bootstrap || event.source !== window.parent || event.origin !== this.bootstrap.origin) return;
    const message = event.data as Partial<PveBridgeResponse> | null;
    if (
      !message ||
      message.source !== 'pvn-loader' ||
      message.type !== 'pvn:pve:response' ||
      message.version !== 1 ||
      message.nonce !== this.bootstrap.nonce ||
      typeof message.id !== 'string'
    ) {
      return;
    }
    const pending = this.pending.get(message.id);
    if (!pending) return;
    window.clearTimeout(pending.timer);
    this.pending.delete(message.id);
    if (message.ok === true) pending.resolve(message.data);
    else pending.reject(new Error(message.error?.message || 'PVE request failed'));
  }
}

export const pveBridge = new PveBridge();
