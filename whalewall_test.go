package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	"github.com/google/go-cmp/cmp"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/matryer/is"
	"go.uber.org/zap"
	"go4.org/netipx"
	"golang.org/x/sys/unix"
)

func TestIntegration(t *testing.T) {
	is := is.New(t)

	is.True(run(t, "docker", "compose", "-f=testdata/docker-compose.yml", "up", "-d") == 0)
	t.Cleanup(func() {
		run(t, "docker", "compose", "-f=testdata/docker-compose.yml", "down")
	})

	logger, err := zap.NewDevelopment()
	is.NoErr(err)

	ctx, cancel := context.WithCancel(context.Background())
	r := newRuleManager(logger)
	err = r.start(ctx, t.TempDir())
	is.NoErr(err)
	t.Cleanup(func() {
		err = r.clearRules(ctx)
		if err != nil {
			t.Logf("error cleaning rules: %v", err)
		}
		cancel()
		r.stop()
	})

	// wait until whalewall has created firewall rules
	time.Sleep(time.Second)

	is.True(runCmd(t, "client", "nslookup google.com") == 0)                             // udp port 53 is allowed
	is.True(runCmd(t, "client", "curl --connect-timeout 1 http://1.1.1.1") == 0)         // tcp port 80 to 1.1.1.1 is allowed
	is.True(runCmd(t, "client", "curl --connect-timeout 1 http://1.0.0.1") != 0)         // tcp port 80 to 1.0.0.1 is not allowed
	is.True(runCmd(t, "client", "curl --connect-timeout 1 https://www.google.com") == 0) // DNS and HTTPS is allowed externally
	is.True(portOpen(t, "client", "server", 9001, false))                                // tcp port 9001 is allowed client -> server
	is.True(!portOpen(t, "client", "server", 9001, true))                                // udp port 9001 is not allowed client -> server
	is.True(!portOpen(t, "client", "server", 80, false))                                 // tcp port 80 is not allowed client -> server
	is.True(!portOpen(t, "client", "server", 80, true))                                  // udp port 80 is not allowed client -> server
	is.True(portOpen(t, "tester", "localhost", 8080, false))                             // tcp mapped port 8080:80 of client is allowed from localhost
	is.True(!portOpen(t, "tester", "localhost", 8080, true))                             // udp mapped port 8080:80 of client is not allowed from localhost
	is.True(!portOpen(t, "tester", "localhost", 8081, false))                            // tcp mapped port 8081:80 of server is not allowed from localhost
	is.True(!portOpen(t, "tester", "localhost", 8081, true))                             // udp mapped port 8081:80 of server is not allowed from localhost
	is.True(!portOpen(t, "tester", "localhost", 9001, false))                            // tcp mapped port 9001:9001 of server is not allowed from localhost
	is.True(!portOpen(t, "tester", "localhost", 9001, true))                             // udp mapped port 9001:9001 of server is not allowed from localhost
}

func portOpen(t *testing.T, container, host string, port uint16, udp bool) bool {
	var udpFlag string
	if udp {
		udpFlag = "-sU"
	}
	// use nmap to determine if port is open and grep for "open" not "open|filtered"
	nmapCmd := fmt.Sprintf(`nmap -n -p %d %s %s 2>&1 | egrep "open\s"`, port, udpFlag, host)

	return runCmd(t, container, nmapCmd) == 0
}

func runCmd(t *testing.T, container, command string) int {
	args := []string{
		"docker",
		"compose",
		"-f=testdata/docker-compose.yml",
		"exec",
		"-T",
		container,
		"sh",
		"-c",
		command,
	}

	return run(t, args...)
}

func run(t *testing.T, args ...string) int {
	t.Logf("running %v", args)

	cmd := exec.Command(args[0], args[1:]...)
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		t.Fatalf("error running command %v: %v", args, err)
	}

	return 0
}

func TestRuleCreation(t *testing.T) {
	cont1ID := "container_one_ID"
	cont1Name := "container1"
	gatewayAddr := netip.MustParseAddr("172.0.1.1")
	cont1Addr := netip.MustParseAddr("172.0.1.2")
	dstAddr := netip.MustParseAddr("1.1.1.1")
	dstRange := netipx.RangeOfPrefix(netip.MustParsePrefix("192.168.1.0/24"))
	lowDstAddr := dstRange.From()
	highDstAddr := dstRange.To()

	tests := []struct {
		name          string
		container     types.ContainerJSON
		expectedRules map[*nftables.Chain][]*nftables.Rule
	}{
		{
			name: "deny all",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
					},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "allow HTTPS outbound",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
output:
  - proto: tcp
    port: 443`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "allow HTTPS outbound to 1.1.1.1",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
output:
  - ip: 1.1.1.1
    proto: tcp
    port: 443`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(dstAddr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(dstAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "allow DNS outbound to 192.168.1.0/24",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
output:
  - ip: 192.168.1.0/24
    proto: udp
    port: 53`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchIPRangeExprs(lowDstAddr, highDstAddr, dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(53, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPRangeExprs(lowDstAddr, highDstAddr, srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(53, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "verdict with log prefix",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
output:
  - log_prefix: "logger pfx"
    proto: tcp
    port: 443`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								logExpr(formatLogPrefix("logger pfx", cont1Name, cont1ID)),
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "verdict with queue",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
output:
  - proto: tcp
    port: 443
    verdict:
      queue: 1000`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1000,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "verdict with queue and est queues",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
output:
  - proto: tcp
    port: 443
    verdict:
      queue: 1000
      input_est_queue: 1001
      output_est_queue: 1002`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1000,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1002,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1001,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "verdict with queue and same output est queue",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
output:
  - proto: tcp
    port: 443
    verdict:
      queue: 1000
      input_est_queue: 1001
      output_est_queue: 1000`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1000,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1001,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "allow external access to mapped ports",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
mapped_ports:
  external:
    allow: true`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					NetworkSettingsBase: types.NetworkSettingsBase{
						Ports: nat.PortMap{
							"80/tcp": []nat.PortBinding{
								{
									HostIP:   "0.0.0.0",
									HostPort: "80",
								},
								{
									HostIP:   "::",
									HostPort: "80",
								},
							},
							"53/udp": []nat.PortBinding{
								{
									HostIP:   "0.0.0.0",
									HostPort: "5533",
								},
								{
									HostIP:   "::",
									HostPort: "5533",
								},
							},
						},
					},
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				whalewallChain: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(localAddr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(5533, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								dropVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(localAddr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(80, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								dropVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					srcJumpRule,
					dstJumpRule,
				},
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(gatewayAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(53, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								dropVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(53, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(53, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(gatewayAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(80, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								dropVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(80, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(80, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},

		{
			name: "allow access from 192.168.1.0/24 to mapped ports",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
mapped_ports:
  external:
    allow: true
    ip: 192.168.1.0/24`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					NetworkSettingsBase: types.NetworkSettingsBase{
						Ports: nat.PortMap{
							"80/tcp": []nat.PortBinding{
								{
									HostIP:   "0.0.0.0",
									HostPort: "8080",
								},
								{
									HostIP:   "::",
									HostPort: "8080",
								},
							},
							"53/udp": []nat.PortBinding{
								{
									HostIP:   "0.0.0.0",
									HostPort: "5533",
								},
								{
									HostIP:   "::",
									HostPort: "5533",
								},
							},
						},
					},
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(gatewayAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(53, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								dropVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPRangeExprs(lowDstAddr, highDstAddr, srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(53, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchIPRangeExprs(lowDstAddr, highDstAddr, dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(53, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(gatewayAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(80, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								dropVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPRangeExprs(lowDstAddr, highDstAddr, srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(80, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchIPRangeExprs(lowDstAddr, highDstAddr, dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_TCP),
							matchPortExprs(80, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "allow localhost access to mapped ports",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
mapped_ports:
  localhost:
    allow: true`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					NetworkSettingsBase: types.NetworkSettingsBase{
						Ports: nat.PortMap{
							"443/udp": []nat.PortBinding{
								{
									HostIP:   "0.0.0.0",
									HostPort: "8443",
								},
							},
						},
					},
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(gatewayAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(gatewayAddr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "allow localhost access to mapped ports with queue",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
mapped_ports:
  localhost:
    allow: true
    verdict:
      queue: 1000`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					NetworkSettingsBase: types.NetworkSettingsBase{
						Ports: nat.PortMap{
							"443/udp": []nat.PortBinding{
								{
									HostIP:   "0.0.0.0",
									HostPort: "8443",
								},
							},
						},
					},
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(gatewayAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNew),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1000,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(gatewayAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(gatewayAddr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								acceptVerdict,
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
		{
			name: "allow localhost access to mapped ports with same input est queue",
			container: types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:   cont1ID,
					Name: "/" + cont1Name,
				},
				Config: &container.Config{
					Labels: map[string]string{
						enabledLabel: "true",
						rulesLabel: `
mapped_ports:
  localhost:
    allow: true
    verdict:
      queue: 1000
      input_est_queue: 1000
      output_est_queue: 1001`,
					},
				},
				NetworkSettings: &types.NetworkSettings{
					NetworkSettingsBase: types.NetworkSettingsBase{
						Ports: nat.PortMap{
							"443/udp": []nat.PortBinding{
								{
									HostIP:   "0.0.0.0",
									HostPort: "8443",
								},
							},
						},
					},
					Networks: map[string]*network.EndpointSettings{
						"default": {
							Gateway:   gatewayAddr.String(),
							IPAddress: cont1Addr.String(),
						},
					},
				},
			},
			expectedRules: map[*nftables.Chain][]*nftables.Rule{
				{
					Name:  buildChainName(cont1Name, cont1ID),
					Table: filterTable,
				}: {
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(gatewayAddr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(cont1Addr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(443, dstPortOffset),
							matchConnStateExprs(stateNewEst),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1000,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					{
						Exprs: slicesJoin(
							matchIPExprs(ref(cont1Addr.As4())[:], srcAddrOffset),
							matchIPExprs(ref(gatewayAddr.As4())[:], dstAddrOffset),
							matchProtoExprs(unix.IPPROTO_UDP),
							matchPortExprs(443, srcPortOffset),
							matchConnStateExprs(stateEst),
							[]expr.Any{
								&expr.Counter{},
								&expr.Queue{
									Num: 1001,
								},
							},
						),
						UserData: []byte(cont1ID),
					},
					createDropRule(
						&nftables.Chain{
							Name:  buildChainName(cont1Name, cont1ID),
							Table: filterTable,
						},
						cont1ID,
					),
				},
			},
		},
	}

	is := is.New(t)
	logger, err := zap.NewDevelopment()
	is.NoErr(err)

	comparer := func(r1, r2 *nftables.Rule) bool {
		return rulesEqual(logger, r1, r2)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := is.New(t)

			// create new database and base rules
			r := newRuleManager(zap.NewNop())
			err = r.init(context.Background(), t.TempDir())
			is.NoErr(err)
			err = r.createBaseRules()
			is.NoErr(err)
			t.Cleanup(func() {
				err := r.clearRules(context.Background())
				is.NoErr(err)
			})

			// create rules
			err := r.createContainerRules(context.Background(), tt.container)
			is.NoErr(err)

			// TODO: create rules in network namespace
			nfc, err := nftables.New()
			is.NoErr(err)

			// check that created rules are what is expected
			for chain, expectedRules := range tt.expectedRules {
				rules, err := nfc.GetRules(chain.Table, chain)
				is.NoErr(err)

				is.Equal(len(expectedRules), len(rules))
				for i := range expectedRules {
					// set rule's table here so we don't have to in test
					// cases above
					expectedRules[i].Table = chain.Table

					if !cmp.Equal(expectedRules[i], rules[i], cmp.Comparer(comparer)) {
						t.Errorf("chain %s rule %d not equal:\n%s", chain.Name, i, cmp.Diff(expectedRules[i].Exprs, rules[i].Exprs))
					}
				}
			}
		})
	}
}

func slicesJoin[T any](s ...[]T) (ret []T) {
	for _, ss := range s {
		ret = append(ret, ss...)
	}

	return ret
}
