package tunnel

import (
	"context"
	"net/url"

	"github.com/WaferJay/adbgroup/internal/gateway"
)

// ListGroups returns only the groups authorized for this client's identity.
func (c *Client) ListGroups(ctx context.Context) ([]gateway.Group, error) {
	var groups []gateway.Group
	err := c.request(ctx, "GET", "/v1/groups", nil, &groups)
	return groups, err
}

// ListDevices lists a group's inventory without creating a tunnel or listener.
// An empty group is resolved automatically when exactly one group is authorized.
func (c *Client) ListDevices(ctx context.Context, group string) ([]gateway.Device, error) {
	group, err := c.resolveGroup(ctx, group)
	if err != nil {
		return nil, err
	}
	var devices []gateway.Device
	err = c.request(ctx, "GET", "/v1/groups/"+url.PathEscape(group)+"/devices", nil, &devices)
	return devices, err
}

// CloseIdleConnections releases reusable HTTP connections after API queries.
func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}
