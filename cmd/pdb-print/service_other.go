//go:build !windows

package main

import (
	"context"
	"errors"
)

func runService(context.Context, string) error {
	return errors.New("pdb-print service run is what the Windows service starts; elsewhere use pdb-print serve")
}
