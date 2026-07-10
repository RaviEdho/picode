//go:build !linux

package main

import "io"

func newPlatformLineEditor(io.Reader, io.Writer) lineEditor { return nil }
