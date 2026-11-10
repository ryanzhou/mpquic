//go:build !linux

package app

import "syscall"

// socketMark: firewall marks are a Linux feature.
func socketMark(uint32) func(network, address string, c syscall.RawConn) error { return nil }
