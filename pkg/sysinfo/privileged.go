package sysinfo

import (
	"context"

	"lightpanel/pkg/helper"
)

// PrivilegedCall, when non-nil, forwards root-level operations to the
// separate privileged helper process. main wires it up only when the panel
// itself runs unprivileged and a [helper] config section is present; a root
// panel leaves it nil and executes privileged commands directly, exactly as
// before. Every helper request is re-validated and ACL-checked on the
// helper side, so this is a transport choice, not the authorization boundary.
var PrivilegedCall func(context.Context, helper.Request) (string, error)

// UpdateStagingDir is where an unprivileged panel downloads release assets
// before the helper verifies and installs them. Empty disables the path.
var UpdateStagingDir string

// privileged is a small helper so handlers fail closed with a clear error
// when the panel is unprivileged but no helper is configured.
func privileged(ctx context.Context, req helper.Request) (string, bool, error) {
	if PrivilegedCall == nil {
		return "", false, nil
	}
	out, err := PrivilegedCall(ctx, req)
	return out, true, err
}
