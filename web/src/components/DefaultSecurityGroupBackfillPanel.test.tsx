import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { ApiError, type ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import type {
  DefaultSecurityGroupBackfillPlan,
  DefaultSecurityGroupBackfillReport,
} from '../api/types';
import { DefaultSecurityGroupBackfillPanel } from './DefaultSecurityGroupBackfillPanel';

const projectID = '11111111-1111-4111-8111-111111111111';
const unavailableProjectID = '22222222-2222-4222-8222-222222222222';
const portID = '33333333-3333-4333-8333-333333333333';
const unavailablePortID = '44444444-4444-4444-8444-444444444444';

const plan: DefaultSecurityGroupBackfillPlan = {
  cluster: 'pve-lab',
  generated_at: '2026-08-06T01:00:00Z',
  warning: `machine warning ${portID}`,
  total_legacy_ports: 3,
  total_attached_ports: 1,
  can_apply: true,
  projects: [
    {
      project_id: projectID,
      project_name: 'Tenant A',
      default_security_group_id: '55555555-5555-4555-8555-555555555555',
      default_security_group_name: 'default',
      default_ready: true,
      legacy_ports: [
        {
          port_id: portID,
          port_name: 'frontend',
          revision: 3,
          attached: true,
          node_id: '66666666-6666-4666-8666-666666666666',
          node_name: 'pve-a',
          vmid: 100,
          nic: 'net0',
        },
        {
          port_id: unavailablePortID,
          port_name: unavailablePortID,
          revision: 1,
          attached: false,
        },
      ],
    },
    {
      project_id: unavailableProjectID,
      project_name: unavailableProjectID,
      default_security_group_id: '77777777-7777-4777-8777-777777777777',
      default_security_group_name: 'default',
      default_ready: true,
      legacy_ports: [{
        port_id: '88888888-8888-4888-8888-888888888888',
        port_name: 'database',
        revision: 2,
        attached: false,
      }],
    },
    {
      project_id: '99999999-9999-4999-8999-999999999999',
      project_name: 'No legacy ports',
      default_security_group_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
      default_security_group_name: 'default',
      default_ready: false,
      blocked_reason: 'default_name_collision',
      legacy_ports: [],
    },
  ],
};

const dryRun: DefaultSecurityGroupBackfillReport = {
  cluster: 'pve-lab',
  dry_run: true,
  warning: `machine warning ${portID}`,
  planned: 3,
  migrated: 0,
  skipped: 0,
  failed: 1,
  results: [{
    project_id: unavailableProjectID,
    port_id: '88888888-8888-4888-8888-888888888888',
    port_name: 'database',
    attached: false,
    status: 'failed',
    revision_before: 2,
    detail: `blocked resource ${portID}`,
  }],
};

const partialApply: DefaultSecurityGroupBackfillReport = {
  cluster: 'pve-lab',
  dry_run: false,
  warning: `machine warning ${portID}`,
  planned: 3,
  migrated: 2,
  skipped: 0,
  failed: 1,
  results: [{
    project_id: unavailableProjectID,
    port_id: '88888888-8888-4888-8888-888888888888',
    port_name: 'database',
    attached: false,
    status: 'failed',
    revision_before: 2,
    error: `failed resource ${portID}`,
  }],
};

describe('DefaultSecurityGroupBackfillPanel', () => {
  it('shows human candidates, warns for attached traffic, and gates apply on dry-run plus exact cluster confirmation', async () => {
    const emptyPlan = { ...plan, total_legacy_ports: 0, total_attached_ports: 0, projects: [] };
    const defaultSecurityGroupBackfillPlan = vi.fn()
      .mockResolvedValueOnce(plan)
      .mockResolvedValueOnce(emptyPlan);
    const applyDefaultSecurityGroupBackfill = vi.fn()
      .mockResolvedValueOnce(dryRun)
      .mockResolvedValueOnce(partialApply);

    render(
      <ApiProvider client={{ defaultSecurityGroupBackfillPlan, applyDefaultSecurityGroupBackfill } as unknown as ApiClient}>
        <DefaultSecurityGroupBackfillPanel />
      </ApiProvider>,
    );

    const panel = await screen.findByRole('region', { name: 'Legacy security policy backfill' });
    expect(within(panel).getByText('frontend')).toBeInTheDocument();
    expect(within(panel).getAllByText('Tenant A')).toHaveLength(2);
    expect(within(panel).getByText('Unavailable port')).toBeInTheDocument();
    expect(within(panel).getByText('Unavailable project')).toBeInTheDocument();
    expect(within(panel).queryByText('No legacy ports')).not.toBeInTheDocument();
    expect(within(panel).getByText('VM 100 · net0 on pve-a')).toBeInTheDocument();
    expect(within(panel).getByText('Attached traffic will change immediately')).toBeInTheDocument();
    expect(panel).not.toHaveTextContent(projectID);
    expect(panel).not.toHaveTextContent(unavailableProjectID);
    expect(panel).not.toHaveTextContent(portID);
    expect(panel).not.toHaveTextContent(unavailablePortID);

    fireEvent.click(within(panel).getByRole('button', { name: 'Dry-run' }));
    expect(await within(panel).findByText('Dry-run complete')).toBeInTheDocument();
    expect(applyDefaultSecurityGroupBackfill).toHaveBeenNthCalledWith(1);

    const applyButton = within(panel).getByRole('button', { name: 'Apply backfill' });
    const confirmation = within(panel).getByRole('textbox', { name: /Type pve-lab to confirm/ });
    expect(applyButton).toBeDisabled();
    fireEvent.change(confirmation, { target: { value: 'PVE-LAB' } });
    expect(applyButton).toBeDisabled();
    fireEvent.change(confirmation, { target: { value: 'pve-lab' } });
    expect(applyButton).toBeEnabled();
    fireEvent.click(applyButton);

    expect(await within(panel).findByText('Backfill complete')).toBeInTheDocument();
    expect(within(panel).getByText('2 migrated · 0 skipped · 1 failed')).toBeInTheDocument();
    expect(within(panel).getByText('database · Unavailable project')).toBeInTheDocument();
    expect(within(panel).getByText('complete')).toBeInTheDocument();
    expect(applyDefaultSecurityGroupBackfill).toHaveBeenNthCalledWith(2, { dry_run: false, confirm: 'pve-lab' });
    await waitFor(() => expect(defaultSecurityGroupBackfillPlan).toHaveBeenCalledTimes(2));
    expect(panel).not.toHaveTextContent(portID);
    expect(panel).not.toHaveTextContent('failed resource');
  });

  it.each([403, 404, 405, 501])('quietly hides the optional panel when the endpoint returns %s', async (status) => {
    const defaultSecurityGroupBackfillPlan = vi.fn().mockRejectedValue(
      new ApiError(`not available ${portID}`, status),
    );

    render(
      <ApiProvider client={{ defaultSecurityGroupBackfillPlan } as unknown as ApiClient}>
        <DefaultSecurityGroupBackfillPanel />
      </ApiProvider>,
    );

    await waitFor(() => expect(defaultSecurityGroupBackfillPlan).toHaveBeenCalledOnce());
    expect(screen.queryByText('Legacy security policy backfill')).not.toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(portID);
  });

  it('hides the panel if dry-run permission is denied after a readable plan', async () => {
    const defaultSecurityGroupBackfillPlan = vi.fn().mockResolvedValue(plan);
    const applyDefaultSecurityGroupBackfill = vi.fn().mockRejectedValue(new ApiError('forbidden', 403));

    render(
      <ApiProvider client={{ defaultSecurityGroupBackfillPlan, applyDefaultSecurityGroupBackfill } as unknown as ApiClient}>
        <DefaultSecurityGroupBackfillPanel />
      </ApiProvider>,
    );

    const panel = await screen.findByRole('region', { name: 'Legacy security policy backfill' });
    fireEvent.click(within(panel).getByRole('button', { name: 'Dry-run' }));
    await waitFor(() => expect(screen.queryByText('Legacy security policy backfill')).not.toBeInTheDocument());
  });

  it('requires a new dry-run after an apply request fails', async () => {
    const defaultSecurityGroupBackfillPlan = vi.fn().mockResolvedValue(plan);
    const applyDefaultSecurityGroupBackfill = vi.fn()
      .mockResolvedValueOnce(dryRun)
      .mockRejectedValueOnce(new ApiError(`conflict ${portID}`, 409));

    render(
      <ApiProvider client={{ defaultSecurityGroupBackfillPlan, applyDefaultSecurityGroupBackfill } as unknown as ApiClient}>
        <DefaultSecurityGroupBackfillPanel />
      </ApiProvider>,
    );

    const panel = await screen.findByRole('region', { name: 'Legacy security policy backfill' });
    fireEvent.click(within(panel).getByRole('button', { name: 'Dry-run' }));
    const confirmation = await within(panel).findByRole('textbox', { name: /Type pve-lab to confirm/ });
    fireEvent.change(confirmation, { target: { value: 'pve-lab' } });
    fireEvent.click(within(panel).getByRole('button', { name: 'Apply backfill' }));

    expect(await within(panel).findByText('The backfill could not be applied. Run a new dry-run before trying again.')).toBeInTheDocument();
    expect(within(panel).queryByRole('button', { name: 'Apply backfill' })).not.toBeInTheDocument();
    expect(panel).not.toHaveTextContent(portID);
  });
});
