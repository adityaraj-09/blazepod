// Layer: host-agent internal — WireGuard per-tenant tunnel manager (Linux).
// Each enterprise tenant gets a dedicated WireGuard interface (e.g. wg-ten-abc123).
// All sandbox traffic for that tenant is routed through the tenant's WireGuard
// endpoint, providing cryptographic isolation on top of the eBPF egress filters.
//
// Cryptographic isolation guarantee:
//   Even if an eBPF rule is misconfigured, traffic from tenant A cannot reach
//   tenant B without possessing tenant B's WireGuard private key.
//
// Requires: kernel WireGuard module (CONFIG_WIREGUARD=m/y, kernel >= 5.6)
//           or wireguard-go in userspace; CAP_NET_ADMIN.
//go:build linux

package wireguard

import (
	"encoding/base64"
	"fmt"
	"net"
	"os/exec"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// TenantTunnel holds the WireGuard configuration for one tenant.
type TenantTunnel struct {
	TenantID string
	// Interface is the WireGuard interface name on this host (e.g. "wg-ten-abc123").
	Interface string
	// PrivateKey is the host's WireGuard private key for this tenant (base64 encoded).
	PrivateKey string
	// PeerPublicKey is the tenant endpoint's WireGuard public key (base64 encoded).
	PeerPublicKey string
	// Endpoint is the tenant's WireGuard peer endpoint address ("IP:port").
	Endpoint string
	// AllowedIPs is the set of CIDRs routed through this tunnel.
	AllowedIPs []string
	// ListenPort is the UDP port the WireGuard interface listens on.
	ListenPort int
}

// Manager manages WireGuard interfaces per tenant using wgctrl.
type Manager struct {
	tunnels map[string]*TenantTunnel
	wg      *wgctrl.Client
}

// NewManager creates a WireGuard Manager. Returns an error if the wgctrl
// kernel interface cannot be opened (missing WireGuard kernel module).
func NewManager() (*Manager, error) {
	wg, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wireguard: open wgctrl: %w", err)
	}
	return &Manager{
		tunnels: make(map[string]*TenantTunnel),
		wg:      wg,
	}, nil
}

// EnsureTunnel creates or reconfigures the WireGuard interface for a tenant.
// Idempotent: safe to call multiple times with the same or updated configuration.
func (m *Manager) EnsureTunnel(t *TenantTunnel) error {
	// 1. Create the WireGuard interface if it does not exist.
	if err := m.ensureInterface(t.Interface); err != nil {
		return fmt.Errorf("wireguard: ensure interface %s: %w", t.Interface, err)
	}

	// 2. Parse and validate keys.
	privKey, err := parseKey(t.PrivateKey)
	if err != nil {
		return fmt.Errorf("wireguard: parse private key for tenant %s: %w", t.TenantID, err)
	}

	peerPubKey, err := parseKey(t.PeerPublicKey)
	if err != nil {
		return fmt.Errorf("wireguard: parse peer public key for tenant %s: %w", t.TenantID, err)
	}

	// 3. Parse allowed IPs.
	allowedIPNets := make([]net.IPNet, 0, len(t.AllowedIPs))
	for _, cidr := range t.AllowedIPs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("wireguard: parse allowed IP %s: %w", cidr, err)
		}
		allowedIPNets = append(allowedIPNets, *ipNet)
	}

	// 4. Resolve peer endpoint.
	var peerEndpoint *net.UDPAddr
	if t.Endpoint != "" {
		peerEndpoint, err = net.ResolveUDPAddr("udp", t.Endpoint)
		if err != nil {
			return fmt.Errorf("wireguard: resolve endpoint %s: %w", t.Endpoint, err)
		}
	}

	// 5. Apply config via wgctrl.
	cfg := wgtypes.Config{
		PrivateKey:   &privKey,
		ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{
			{
				PublicKey:                   peerPubKey,
				Endpoint:                    peerEndpoint,
				AllowedIPs:                  allowedIPNets,
				PersistentKeepaliveInterval: nil,
				ReplaceAllowedIPs:           true,
			},
		},
	}
	if t.ListenPort > 0 {
		cfg.ListenPort = &t.ListenPort
	}

	if err := m.wg.ConfigureDevice(t.Interface, cfg); err != nil {
		return fmt.Errorf("wireguard: configure device %s: %w", t.Interface, err)
	}

	// 6. Bring the interface up.
	link, err := netlink.LinkByName(t.Interface)
	if err != nil {
		return fmt.Errorf("wireguard: get link %s: %w", t.Interface, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("wireguard: link up %s: %w", t.Interface, err)
	}

	// 7. Add routes for all AllowedIPs → WireGuard interface.
	for _, ipNet := range allowedIPNets {
		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &ipNet,
		}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("wireguard: add route %s via %s: %w", ipNet.String(), t.Interface, err)
		}
	}

	m.tunnels[t.TenantID] = t
	return nil
}

// RemoveTunnel tears down the WireGuard interface for a tenant.
func (m *Manager) RemoveTunnel(tenantID string) error {
	t, ok := m.tunnels[tenantID]
	if !ok {
		return fmt.Errorf("wireguard: no tunnel for tenant %s", tenantID)
	}
	delete(m.tunnels, tenantID)

	link, err := netlink.LinkByName(t.Interface)
	if err != nil {
		return fmt.Errorf("wireguard: get link %s: %w", t.Interface, err)
	}
	return netlink.LinkDel(link)
}

// Stats returns the current WireGuard device statistics for a tenant tunnel.
func (m *Manager) Stats(tenantID string) (*wgtypes.Device, error) {
	t, ok := m.tunnels[tenantID]
	if !ok {
		return nil, fmt.Errorf("wireguard: no tunnel for tenant %s", tenantID)
	}
	return m.wg.Device(t.Interface)
}

// Close releases the wgctrl client.
func (m *Manager) Close() error {
	return m.wg.Close()
}

// ensureInterface creates the WireGuard netlink interface if absent.
func (m *Manager) ensureInterface(name string) error {
	_, err := netlink.LinkByName(name)
	if err == nil {
		return nil // already exists
	}
	// Use `ip link add ... type wireguard` as the fallback if netlink attr type isn't registered.
	// This handles both kernel-native and wireguard-go (userspace) scenarios.
	out, err := exec.Command("ip", "link", "add", name, "type", "wireguard").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link add %s wireguard: %w (%s)", name, err, out)
	}
	return nil
}

// parseKey decodes a WireGuard key from base64.
func parseKey(b64 string) (wgtypes.Key, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("base64 decode: %w", err)
	}
	if len(raw) != wgtypes.KeyLen {
		return wgtypes.Key{}, fmt.Errorf("key must be %d bytes, got %d", wgtypes.KeyLen, len(raw))
	}
	var key wgtypes.Key
	copy(key[:], raw)
	return key, nil
}
