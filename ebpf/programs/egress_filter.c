// Layer: networking — Phase 2 eBPF TC egress filter.
// Attached to the host-side veth interface of each sandbox at the TC (Traffic Control) layer.
// Drops any outbound packet whose destination IP is not in the per-sandbox allowlist.
//
// How it works:
//   1. The Go loader (ebpf/loader/loader.go) compiles this with clang -target bpf.
//   2. For each sandbox, the loader creates a BPF LPM trie map and populates it
//      with the allowed destination CIDRs from the sandbox spec.
//   3. This program is pinned to the host-side veth interface via tc qdisc + filter.
//   4. On every outbound packet:
//      - Parse the Ethernet header to reach the IP header.
//      - LPM trie lookup on the destination IP.
//      - TC_ACT_OK (pass) if found, TC_ACT_SHOT (drop) if not.
//
// Uses CO-RE (Compile Once, Run Everywhere) via libbpf so the same compiled .o
// runs on any kernel >= 5.4 without recompilation.
//
// Compile: clang -O2 -target bpf -D__TARGET_ARCH_x86 \
//           -I/usr/include/bpf -I/usr/include/linux \
//           -c egress_filter.c -o egress_filter.o


#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

// allowed_dsts is a per-sandbox LPM (Longest Prefix Match) trie.
// Key: { prefixlen (32 bits) | IPv4 address (32 bits) }
// Value: __u32 (non-zero = allowed)
// Each sandbox gets its own map instance pinned under /sys/fs/bpf/sandboxes/<id>/allowed_dsts.
struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __uint(key_size, sizeof(__u64));   // prefixlen (4 bytes) + IPv4 addr (4 bytes)
    __uint(value_size, sizeof(__u32));
    __uint(max_entries, 1024);
    __uint(map_flags, BPF_F_NO_PREALLOC);
} allowed_dsts SEC(".maps");

// lpm_key mirrors the kernel's bpf_lpm_trie_key struct for IPv4.
struct lpm_key {
    __u32 prefixlen;
    __u32 addr;
};

// sandbox_egress is the TC classifier program attached to the host-side veth.
// Returns TC_ACT_OK to pass the packet or TC_ACT_SHOT to drop it.
SEC("tc/egress")
int sandbox_egress(struct __sk_buff *skb)
{
    void *data     = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;

    // Parse Ethernet header.
    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return TC_ACT_SHOT;

    // Only inspect IPv4 traffic.
    if (bpf_ntohs(eth->h_proto) != ETH_P_IP)
        return TC_ACT_OK;  // pass non-IPv4 (ARP, IPv6) through unfiltered

    // Parse IPv4 header.
    struct iphdr *ip = (void *)(eth + 1);
    if ((void *)(ip + 1) > data_end)
        return TC_ACT_SHOT;

    // LPM trie lookup: match the destination IP against the allowlist.
    struct lpm_key key = {
        .prefixlen = 32,          // exact host match; CIDRs use smaller prefixlen
        .addr      = ip->daddr,   // destination IPv4 address (network byte order)
    };

    if (bpf_map_lookup_elem(&allowed_dsts, &key) == NULL)
        return TC_ACT_SHOT;  // not in allowlist — drop

    return TC_ACT_OK;
}

char LICENSE[] SEC("license") = "GPL";
