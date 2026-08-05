package main

import (
	"fmt"

	"github.com/pvnstack/proxmox-ovn/internal/buildinfo"
)

func main() {
	fmt.Printf("pvnctl %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
}
