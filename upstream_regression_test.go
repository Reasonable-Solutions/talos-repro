// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package network

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/jsimonetti/rtnetlink/v2"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	internalbgp "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/network/internal/bgp"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
)

// Feed a deterministic resource ordering into one real controller iteration.
// Only the resource store/event source is stubbed: netlink uses the real kernel.
// The same methods exist in the stock and patched controller.
type reproRuntime struct {
	controller.Runtime
	items  []resource.Resource
	events chan controller.ReconcileEvent
	cancel context.CancelFunc
}

func (r *reproRuntime) EventCh() <-chan controller.ReconcileEvent { return r.events }
func (r *reproRuntime) QueueReconcile()                           {}
func (r *reproRuntime) ResetRestartBackoff()                      { r.cancel() }
func (r *reproRuntime) List(context.Context, resource.Kind, ...state.ListOption) (resource.List, error) {
	return resource.List{Items: r.items}, nil
}
func (*reproRuntime) AddFinalizer(context.Context, resource.Pointer, ...resource.Finalizer) error {
	return nil
}
func (*reproRuntime) RemoveFinalizer(context.Context, resource.Pointer, ...resource.Finalizer) error {
	return nil
}

type routeFixture struct {
	conn    *rtnetlink.Conn
	links   []rtnetlink.LinkMessage
	indices [2]uint32
}

func newRouteFixture(t *testing.T) *routeFixture {
	t.Helper()
	conn, err := rtnetlink.Dial(nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	f := &routeFixture{conn: conn}
	for i, name := range []string{"repro0", "repro1"} {
		require.NoError(t, conn.Link.New(&rtnetlink.LinkMessage{Type: unix.ARPHRD_ETHER, Flags: unix.IFF_UP, Change: unix.IFF_UP,
			Attributes: &rtnetlink.LinkAttributes{Name: name, Info: &rtnetlink.LinkInfo{Kind: "dummy"}}}))
		iface, err := net.InterfaceByName(name)
		require.NoError(t, err)
		index := uint32(iface.Index)
		f.indices[i] = index
		t.Cleanup(func() { require.NoError(t, conn.Link.Delete(index)) })
		addr := net.ParseIP([]string{"fe80::a", "fe80::b"}[i])
		require.NoError(t, conn.Address.New(&rtnetlink.AddressMessage{Family: unix.AF_INET6, PrefixLength: 64, Index: index,
			Flags: unix.IFA_F_NODAD, Attributes: &rtnetlink.AddressAttributes{Address: addr, Local: addr}}))
	}
	f.links, err = conn.Link.List()
	require.NoError(t, err)
	return f
}
func (f *routeFixture) add(t *testing.T, protocol uint8, index uint32, metric uint32, gateway string) {
	t.Helper()
	require.NoError(t, f.conn.Route.Add(&rtnetlink.RouteMessage{Family: unix.AF_INET6, Table: unix.RT_TABLE_MAIN,
		Protocol: protocol, Type: unix.RTN_UNICAST, Attributes: rtnetlink.RouteAttributes{
			Gateway: net.ParseIP(gateway), OutIface: index, Priority: metric}}))
}
func (f *routeFixture) routes(t *testing.T) []rtnetlink.RouteMessage {
	t.Helper()
	routes, err := f.conn.Route.List()
	require.NoError(t, err)
	return routes
}
func (f *routeFixture) count(t *testing.T, protocol uint8, index uint32) int {
	t.Helper()
	count := 0
	for _, r := range f.routes(t) {
		if r.DstLength == 0 && r.Protocol == protocol && r.Attributes.OutIface == index {
			count++
		}
	}
	return count
}
func learnedRoute(id, link, gateway string) *network.RouteSpec {
	r := network.NewRouteSpec(network.NamespaceName, id)
	*r.TypedSpec() = internalbgp.RouteSpec(netip.MustParsePrefix("::/0"),
		[]network.RouteNextHop{{Gateway: netip.MustParseAddr(gateway), OutLinkName: link}}, netip.Addr{}, nethelpers.TableMain)
	return r
}
func runOnce(specs ...*network.RouteSpec) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := &reproRuntime{events: make(chan controller.ReconcileEvent, 1), cancel: cancel}
	for _, spec := range specs {
		r.items = append(r.items, spec)
	}
	r.events <- controller.ReconcileEvent{}
	return (&RouteSpecController{}).Run(ctx, r, zap.NewNop())
}

func TestUpstreamRouteRegression(t *testing.T) {
	// Never silently skip: an unprivileged or host-namespace invocation is not
	// evidence of either reproduction or a fix.
	require.Equal(t, "1", os.Getenv("TALOS_ROUTE_REPRO_NETNS"), "run in a disposable network namespace")
	require.Equal(t, 0, os.Geteuid(), "requires namespace-local CAP_NET_ADMIN")
	current, err := os.Readlink("/proc/self/ns/net")
	require.NoError(t, err)
	parent := os.Getenv("TALOS_ROUTE_REPRO_PARENT_NETNS")
	require.Regexp(t, `^net:\[[0-9]+\]$`, parent, "record the parent network namespace before unshare")
	require.NotEqual(t, parent, current, "refusing the parent network namespace")

	t.Run("RAControl", func(t *testing.T) {
		f := newRouteFixture(t)
		f.add(t, unix.RTPROT_RA, f.indices[1], 1024, "fe80::1")
		bgp := learnedRoute("control", "repro0", "fe80::2")
		bgp.TypedSpec().Priority = 100
		require.NoError(t, runOnce(bgp))
		require.Equal(t, 1, f.count(t, unix.RTPROT_RA, f.indices[1]))
		require.Equal(t, 1, f.count(t, unix.RTPROT_BGP, f.indices[0]))
	})
	t.Run("IPv4MetricControl", func(t *testing.T) {
		spec := internalbgp.RouteSpec(netip.MustParsePrefix("0.0.0.0/0"), nil, netip.Addr{}, nethelpers.TableMain)
		require.Zero(t, spec.Priority, "IPv6 RA policy must not change the IPv4 BGP metric")
	})
	t.Run("IPv4LinkMove", func(t *testing.T) {
		f := newRouteFixture(t)
		for _, index := range f.indices {
			addr := net.ParseIP("10.0.0.2").To4()
			require.NoError(t, f.conn.Address.New(&rtnetlink.AddressMessage{Family: unix.AF_INET, PrefixLength: 24, Index: index,
				Attributes: &rtnetlink.AddressAttributes{Address: addr, Local: addr}}))
		}
		spec := internalbgp.RouteSpec(netip.MustParsePrefix("0.0.0.0/0"),
			[]network.RouteNextHop{{Gateway: netip.MustParseAddr("10.0.0.1"), OutLinkName: "repro0"}}, netip.Addr{}, nethelpers.TableMain)
		// Isolate link matching from any change to the default BGP metric.
		spec.Priority = 100
		id := func(s network.RouteSpecSpec) string {
			return network.RouteID(s.Table, s.Family, s.Destination, s.Priority)
		}
		route := network.NewRouteSpec(network.NamespaceName, id(spec))
		*route.TypedSpec() = spec
		require.NoError(t, runOnce(route))
		require.Equal(t, 1, f.count(t, unix.RTPROT_BGP, f.indices[0]))
		route.TypedSpec().OutLinkName = "repro1"
		require.Equal(t, route.Metadata().ID(), id(*route.TypedSpec()), "IPv4 link changes retain resource identity")
		require.NoError(t, runOnce(route), "replace the old-link IPv4 route before exclusive add")
		require.Equal(t, 0, f.count(t, unix.RTPROT_BGP, f.indices[0]))
		require.Equal(t, 1, f.count(t, unix.RTPROT_BGP, f.indices[1]))
		require.NoError(t, runOnce(route), "reconciliation must stay idempotent after moving")
	})
	t.Run("BGPBesideRA", func(t *testing.T) {
		f := newRouteFixture(t)
		f.add(t, unix.RTPROT_RA, f.indices[1], 1024, "fe80::1")
		err := runOnce(learnedRoute("bgp", "repro0", "fe80::2"))
		if errors.Is(err, unix.EEXIST) {
			t.Error("REPRO_RA_BGP_COLLISION: BGP default conflicts with RA default at kernel metric 1024")
			return
		}
		require.NoError(t, err)
		require.Equal(t, 1, f.count(t, unix.RTPROT_RA, f.indices[1]), "RA fallback must survive")
		require.Equal(t, 1, f.count(t, unix.RTPROT_BGP, f.indices[0]))
	})
	t.Run("NextHopReplacement", func(t *testing.T) {
		f := newRouteFixture(t)
		route := learnedRoute("stable-key", "repro0", "fe80::2")
		route.TypedSpec().Priority = 100
		require.NoError(t, runOnce(route))
		route.TypedSpec().Gateway = netip.MustParseAddr("fe80::1")
		route.TypedSpec().OutLinkName = "repro1"
		require.NoError(t, runOnce(route))
		require.Equal(t, 0, f.count(t, unix.RTPROT_BGP, f.indices[0]))
		require.Equal(t, 1, f.count(t, unix.RTPROT_BGP, f.indices[1]))
		require.NoError(t, runOnce(route))
	})
	// Under #14548 ownership is per kernel key and protocol, not link.
	// Two specs with the same kernel key are merged before this controller.
	for _, link := range []string{"repro0", "missing-link"} {
		t.Run("ForeignProtocolOwnership/"+link, func(t *testing.T) {
			f := newRouteFixture(t)
			f.add(t, unix.RTPROT_RA, f.indices[1], 100, "fe80::1")
			old := learnedRoute("old", link, "fe80::1")
			old.TypedSpec().Priority = 100
			old.Metadata().SetPhase(resource.PhaseTearingDown)
			require.NoError(t, runOnce(old))
			require.Equal(t, 1, f.count(t, unix.RTPROT_RA, f.indices[1]))
		})
	}
	t.Run("IdempotentDelete", func(t *testing.T) {
		f := newRouteFixture(t)
		f.add(t, unix.RTPROT_BGP, f.indices[0], 100, "fe80::2")
		old := learnedRoute("old", "repro0", "fe80::2")
		old.TypedSpec().Priority = 100
		old.Metadata().SetPhase(resource.PhaseTearingDown)
		snapshot := f.routes(t)
		// A link event can remove a route after the controller's snapshot.
		for _, r := range snapshot {
			if r.Protocol == unix.RTPROT_BGP {
				require.NoError(t, f.conn.Route.Delete(&r))
			}
		}
		err := (&RouteSpecController{}).syncRoute(context.Background(), &reproRuntime{}, zap.NewNop(), f.conn, f.links, snapshot, old)
		if errors.Is(err, unix.ESRCH) {
			t.Error("REPRO_STALE_DELETE: an already-removed route aborts reconciliation with ESRCH")
			return
		}
		require.NoError(t, err)
	})
}
