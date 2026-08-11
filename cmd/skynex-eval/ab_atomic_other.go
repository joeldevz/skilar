//go:build !linux

package main

import "fmt"

func exchangeABFiles(_, _ string) error {
	return fmt.Errorf("atomic A/B checkpoint exchange is unsupported on this platform")
}
