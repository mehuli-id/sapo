//go:build !linux

package agent

import (
	"context"
	"errors"
)

// applyACLTree hanya ada di Linux (mode pengembangan di sistem lain tidak
// punya ACL POSIX).
func applyACLTree(context.Context, string, shareACL) error {
	return errors.New("ACL POSIX hanya didukung di Linux")
}
