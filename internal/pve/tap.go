package pve

import (
	"fmt"
	"strconv"
	"strings"
)

// TapName is the identity encoded by a Proxmox QEMU TAP interface name.
// Proxmox names QEMU interfaces tap<vmid>i<nic-index>.
type TapName struct {
	VMID     int
	NICIndex int
}

func (t TapName) String() string {
	return fmt.Sprintf("tap%di%d", t.VMID, t.NICIndex)
}

// ParseTapName parses a Proxmox QEMU TAP interface name. Names for firewalls,
// containers, and other devices are intentionally rejected.
func ParseTapName(name string) (TapName, error) {
	var result TapName
	if !strings.HasPrefix(name, "tap") {
		return result, fmt.Errorf("invalid PVE TAP name %q", name)
	}

	body := strings.TrimPrefix(name, "tap")
	separator := strings.IndexByte(body, 'i')
	if separator <= 0 || separator == len(body)-1 || strings.IndexByte(body[separator+1:], 'i') >= 0 {
		return result, fmt.Errorf("invalid PVE TAP name %q", name)
	}

	vmidText, indexText := body[:separator], body[separator+1:]
	if !decimalDigits(vmidText) || !decimalDigits(indexText) {
		return result, fmt.Errorf("invalid PVE TAP name %q", name)
	}

	vmid, err := strconv.Atoi(vmidText)
	if err != nil || vmid <= 0 {
		return result, fmt.Errorf("invalid VM ID in PVE TAP name %q", name)
	}
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 {
		return result, fmt.Errorf("invalid NIC index in PVE TAP name %q", name)
	}

	result.VMID = vmid
	result.NICIndex = index
	return result, nil
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
