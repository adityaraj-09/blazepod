// Layer: host-agent internal — eBPF TC egress filter loader (Linux).
// Loads the compiled ebpf/programs/egress_filter.o, attaches it to the clsact
// qdisc on the host-side veth of each sandbox, and manages the per-sandbox
// LPM trie map that the kernel program consults to allow/deny egress traffic.
//
// Kernel path:
//   sendmsg() → net/core/filter → TC egress → sandbox_egress (BPF) →
//     lpm_trie lookup(dst_ip) → PASS if found, DROP otherwise
//
// Uses github.com/cilium/ebpf (pure Go, no CGO) which supports CO-RE.
// Requires Linux kernel >= 5.4 and CAP_NET_ADMIN + CAP_BPF.
//go:build linux

package loader

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// lpmKey mirrors the C struct lpm_key in egress_filter.c.
//
//	struct lpm_key { __u32 prefixlen; __u32 addr; };
type lpmKey struct {
	Prefixlen uint32
	Addr      uint32
}

// tcDetacher is an internal interface for detaching an attached TC program.
// link.Link has unexported methods and cannot be implemented externally, so we
// use this narrow interface instead and hold either a cilium link or a netlink filter.
type tcDetacher interface {
	close() error
}

type tcxDetacher struct{ lnk link.Link }

func (t *tcxDetacher) close() error { return t.lnk.Close() }

type netlinkDetacher struct{ filter *netlink.BpfFilter }

func (n *netlinkDetacher) close() error { return netlink.FilterDel(n.filter) }

// EgressFilter manages the lifecycle of a per-sandbox eBPF TC egress filter.
type EgressFilter struct {
	SandboxID string
	HostVeth  string

	objs    *ebpf.Collection
	prog    *ebpf.Program
	trie    *ebpf.Map
	tc      tcDetacher
	ifIndex int
}

// NewEgressFilter creates an EgressFilter for the given sandbox and interface.
func NewEgressFilter(sandboxID, hostVeth string) *EgressFilter {
	return &EgressFilter{SandboxID: sandboxID, HostVeth: hostVeth}
}

// Load attaches the eBPF TC egress program from objectPath to f.HostVeth.
//
// Steps:
//  1. Load the compiled .o via cilium/ebpf LoadCollectionSpec.
//  2. Create the Collection (verifies programs, pins maps).
//  3. Add a clsact qdisc to the host veth (idempotent).
//  4. Attach the egress program (TCX on kernel >= 6.1, netlink fallback).
func (f *EgressFilter) Load(objectPath string) error {
	if _, err := os.Stat(objectPath); err != nil {
		return fmt.Errorf("ebpf loader: object file %s not found: %w", objectPath, err)
	}

	spec, err := ebpf.LoadCollectionSpec(objectPath)
	if err != nil {
		return fmt.Errorf("ebpf loader: LoadCollectionSpec: %w", err)
	}

	objs, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("ebpf loader: NewCollection: %w", err)
	}

	prog, ok := objs.Programs["sandbox_egress"]
	if !ok {
		objs.Close()
		return fmt.Errorf("ebpf loader: program 'sandbox_egress' not found in %s", objectPath)
	}

	trie, ok := objs.Maps["allowed_dsts"]
	if !ok {
		objs.Close()
		return fmt.Errorf("ebpf loader: map 'allowed_dsts' not found in %s", objectPath)
	}

	iface, err := net.InterfaceByName(f.HostVeth)
	if err != nil {
		objs.Close()
		return fmt.Errorf("ebpf loader: interface %s not found: %w", f.HostVeth, err)
	}

	if err := ensureClsactQdisc(iface.Index); err != nil {
		objs.Close()
		return fmt.Errorf("ebpf loader: clsact qdisc on %s: %w", f.HostVeth, err)
	}

	tc, err := attachTCEgress(iface.Index, prog)
	if err != nil {
		objs.Close()
		return fmt.Errorf("ebpf loader: attach TC egress on %s: %w", f.HostVeth, err)
	}

	f.objs = objs
	f.prog = prog
	f.trie = trie
	f.tc = tc
	f.ifIndex = iface.Index
	return nil
}

// AddCIDR inserts an allowed destination CIDR into the sandbox's LPM trie map.
// The kernel program passes packets whose destination IP matches any inserted CIDR.
func (f *EgressFilter) AddCIDR(cidr string) error {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("ebpf loader: parse CIDR %s: %w", cidr, err)
	}
	ones, _ := ipNet.Mask.Size()

	ip4 := ipNet.IP.To4()
	if ip4 == nil {
		return fmt.Errorf("ebpf loader: only IPv4 CIDRs are supported (got %s)", cidr)
	}
	addr := binary.BigEndian.Uint32(ip4)

	key := lpmKey{Prefixlen: uint32(ones), Addr: addr}
	var value uint8 = 1

	if err := f.trie.Put(unsafe.Pointer(&key), unsafe.Pointer(&value)); err != nil {
		return fmt.Errorf("ebpf loader: map update for %s: %w", cidr, err)
	}
	return nil
}

// Detach removes the eBPF filter and releases all kernel resources.
func (f *EgressFilter) Detach() error {
	if f.tc != nil {
		if err := f.tc.close(); err != nil {
			return fmt.Errorf("ebpf loader: TC detach: %w", err)
		}
		f.tc = nil
	}
	if f.objs != nil {
		f.objs.Close()
		f.objs = nil
	}
	if f.ifIndex > 0 {
		_ = removeClsactQdisc(f.ifIndex)
	}
	return nil
}

// ---------- netlink helpers ----------

// ensureClsactQdisc adds a clsact qdisc to ifIndex if not already present.
func ensureClsactQdisc(ifIndex int) error {
	lnk, err := netlink.LinkByIndex(ifIndex)
	if err != nil {
		return err
	}
	qdiscs, err := netlink.QdiscList(lnk)
	if err != nil {
		return err
	}
	for _, q := range qdiscs {
		if q.Type() == "clsact" {
			return nil
		}
	}
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: ifIndex,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	return netlink.QdiscAdd(qdisc)
}

// removeClsactQdisc deletes the clsact qdisc from ifIndex (idempotent).
func removeClsactQdisc(ifIndex int) error {
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: ifIndex,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	return netlink.QdiscDel(qdisc)
}

// attachTCEgress attaches prog to the TC egress hook of ifIndex.
// Prefers TCX (kernel >= 6.1); falls back to classic netlink BpfFilter.
func attachTCEgress(ifIndex int, prog *ebpf.Program) (tcDetacher, error) {
	// Preferred: TCX (kernel >= 6.1, supports link.Link natively).
	lnk, err := link.AttachTCX(link.TCXOptions{
		Interface: ifIndex,
		Attach:    ebpf.AttachTCXEgress,
		Program:   prog,
	})
	if err == nil {
		return &tcxDetacher{lnk: lnk}, nil
	}

	// Fallback: classic netlink BpfFilter (kernels 5.4 – 6.0).
	iface, err2 := netlink.LinkByIndex(ifIndex)
	if err2 != nil {
		return nil, fmt.Errorf("netlink link by index: %w", err2)
	}
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: iface.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_EGRESS,
			Handle:    1,
			Protocol:  unix.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           prog.FD(),
		Name:         "sandbox_egress",
		DirectAction: true,
	}
	if err3 := netlink.FilterAdd(filter); err3 != nil {
		return nil, fmt.Errorf("netlink FilterAdd (TCX: %v): %w", err, err3)
	}
	return &netlinkDetacher{filter: filter}, nil
}
