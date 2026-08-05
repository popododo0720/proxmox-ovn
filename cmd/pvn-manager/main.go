package main

import (
	"fmt"

	"github.com/pvnstack/proxmox-ovn/internal/buildinfo"
)

func main() {
	fmt.Printf("pvn-manager %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
}
