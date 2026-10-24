//go:build !linux

package agent

import (
	"errors"
	"io"
	"os"
)

// File Station hanya didukung di Linux (openat2).

func FSHelperMain(io.Reader, io.Writer) int { return 1 }

func socketPair() (*os.File, *os.File, error) { return nil, nil, errors.New("hanya di Linux") }

func recvFile(*os.File) (*os.File, error) { return nil, errors.New("hanya di Linux") }
