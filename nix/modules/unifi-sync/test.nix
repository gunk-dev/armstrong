# Boots a host running the real unit against a fake console, and checks the
# three things that make "diff mode" worth trusting:
#
#   * a site that already matches the instance file reports no changes and the
#     unit succeeds;
#   * a site that has drifted leaves the unit *failed*, with the plan in the
#     journal — drift that only whispered would be drift nobody acts on;
#   * neither run writes. The fake records every request it receives, so this
#     is asserted against what actually crossed the socket rather than against
#     the tool's own account of itself.
#
# It then runs real `unifi sync`s against the fake, which enforces two of the
# console's rules: a network is created into a firewall zone, so a new network
# in a new zone has to arrive as the zone's POST, then the network's POST
# carrying that zone's id; and a TCP_UDP firewall policy is sent as a PRESET
# protocol filter, never a NAMED_PROTOCOL one.
#
# Everything real is real: the module's own systemd unit, the `unifi` package
# this flake builds, `cue` resolving the schema out of the module the
# derivation assembles in the store, and the credential and EnvironmentFile
# plumbing.
#
# `module` is `nixosModules.unifi-sync` — the same value a consumer imports,
# already carrying this flake's packages as its defaults.
{ pkgs, module }:

let
  fixtures = ./test-fixtures.json;

  # The fixture's Default network, as the #Site the zone phase feeds `unifi`.
  defaultNetwork = {
    name = "Default";
    management = "GATEWAY";
    enabled = true;
    vlanId = 1;
    isolationEnabled = false;
    internetAccessEnabled = true;
    cellularBackupEnabled = false;
    mdnsForwardingEnabled = true;
    ipv4 = {
      hostIpAddress = "10.0.0.1";
      prefixLength = 24;
      autoScaleEnabled = false;
      dhcp = {
        mode = "SERVER";
        rangeStart = "10.0.0.10";
        rangeStop = "10.0.0.200";
        leaseTimeSeconds = 86400;
        domainName = "test.invalid";
        pingConflictDetectionEnabled = true;
      };
    };
  };
  # Default as it stands, plus a Lab network in a Lab zone, neither of which
  # exists yet. Sections it omits are left alone.
  labSite = pkgs.writeText "lab-site.json" (
    builtins.toJSON {
      networks = [
        defaultNetwork
        (
          defaultNetwork
          // {
            name = "Lab";
            vlanId = 30;
            ipv4 = defaultNetwork.ipv4 // {
              hostIpAddress = "10.0.30.1";
              dhcp = defaultNetwork.ipv4.dhcp // {
                rangeStart = "10.0.30.10";
                rangeStop = "10.0.30.200";
              };
            };
          }
        )
      ];
      firewallZones = [
        {
          name = "Internal";
          networks = [ "Default" ];
        }
        {
          name = "Lab";
          networks = [ "Lab" ];
        }
      ];
    }
  );
  # A TCP_UDP policy, which the console takes only as a PRESET filter, beside
  # a UDP one, which it takes as a NAMED_PROTOCOL. The DNS policy carries
  # multi-item port, address and connection-state lists, which the console
  # stores in an order of its own. Sections it omits are left alone.
  policySite = pkgs.writeText "policy-site.json" (
    builtins.toJSON {
      firewallPolicies =
        map
          (
            p:
            {
              enabled = true;
              action = "ALLOW";
              allowReturnTraffic = true;
              sourceZone = "Internal";
              destinationZone = "Gateway";
              ipVersion = "IPV4_AND_IPV6";
            }
            // p
          )
          [
            {
              name = "DNS";
              protocol = "TCP_UDP";
              connectionStates = [
                "ESTABLISHED"
                "NEW"
              ];
              destination = {
                type = "IP_ADDRESS";
                ipAddressFilter = {
                  items = [
                    {
                      type = "IP_ADDRESS";
                      value = "192.168.1.4";
                    }
                    {
                      type = "IP_ADDRESS";
                      value = "192.168.1.5";
                    }
                  ];
                  matchOpposite = false;
                };
                portFilter = {
                  items = [
                    "53"
                    "853"
                  ];
                  matchOpposite = false;
                };
              };
            }
            {
              name = "NTP";
              protocol = "UDP";
            }
          ];
    }
  );
  namedTcpUdpPolicy = pkgs.writeText "named-tcp-udp-policy.json" (
    builtins.toJSON {
      name = "refused";
      action.type = "BLOCK";
      source.zoneId = "zone-001";
      destination.zoneId = "zone-002";
      ipProtocolScope = {
        ipVersion = "IPV4_AND_IPV6";
        protocolFilter = {
          type = "NAMED_PROTOCOL";
          protocol.name = "TCP_UDP";
          matchOpposite = false;
        };
      };
    }
  );
  zonelessNetwork = pkgs.writeText "zoneless-network.json" (
    builtins.toJSON {
      name = "Nowhere";
      vlanId = 99;
    }
  );
in
pkgs.testers.runNixOSTest {
  name = "unifi-sync";

  nodes.machine =
    { lib, ... }:
    {
      imports = [ module ];

      # The fake console. It serves from a state file the test rewrites, so
      # "the console drifted" is a genuine change in what the API returns.
      systemd.services.fake-console = {
        description = "Stand-in UniFi Integration API";
        wantedBy = [ "multi-user.target" ];
        serviceConfig = {
          ExecStartPre = "${pkgs.coreutils}/bin/install -m 0644 ${fixtures} /var/lib/fake-console/state.json";
          ExecStart = "${pkgs.python3}/bin/python3 ${./fake-console.py}";
          StateDirectory = "fake-console";
          # World-readable so the test can rewrite the state and read the
          # request log without going through this unit.
          StateDirectoryMode = "0755";
        };
        environment = {
          FAKE_CONSOLE_STATE = "/var/lib/fake-console/state.json";
          FAKE_CONSOLE_REQUESTS = "/var/lib/fake-console/requests.log";
          FAKE_CONSOLE_PORT = "8088";
        };
      };

      modules.unifi-sync = {
        enable = true;
        consoleUrl = "http://127.0.0.1:8088";
        site = "Default";
        instance = ./test-instance;
        mode = "diff";
        apiKeyFile = pkgs.writeText "unifi-api-key" "test-api-key";
        # Matches the passphrase the fixture's SSID carries, which is what
        # makes the clean run clean: `unifi` compares the two. The comment and
        # blank line are there because the real secret is a file a human
        # edits, and the wrapper parses it rather than sourcing it.
        secretsFile = pkgs.writeText "unifi-wifi.env" ''
          # SSID passphrases, by passphraseEnv.

          UNIFI_PSK_TEST=correct-horse-battery
        '';
      };

      # The timer would otherwise fire mid-test on its own schedule.
      systemd.timers.unifi-sync.enable = lib.mkForce false;
    };

  testScript =
    { nodes, ... }:
    let
      unifi = "${nodes.machine.modules.unifi-sync.package}/bin/unifi";
    in
    ''
      machine.wait_for_unit("fake-console.service")
      machine.wait_for_open_port(8088, "127.0.0.1")

      def writes():
          """Every non-GET request the fake console has seen so far."""
          log = machine.succeed("cat /var/lib/fake-console/requests.log")
          return [l for l in log.splitlines() if not l.startswith("GET ")]

      # ---------------------------------------------------------------- clean
      # The console matches the instance file, so the plan is empty and the unit
      # succeeds.
      machine.succeed("systemctl start unifi-sync.service")
      plan = machine.succeed("journalctl -u unifi-sync.service --no-pager")
      assert "OK     network        Default" in plan, plan
      assert "OK     wifi           test-ssid" in plan, plan
      assert "OK     dns policy     A_RECORD host.test.invalid" in plan, plan
      assert "OK     firewall zone  Internal" in plan, plan

      # It really did talk to the console rather than short-circuiting: the
      # detail GETs the overview/detail split forces are in the log.
      requests = machine.succeed("cat /var/lib/fake-console/requests.log")
      assert "GET /proxy/network/integration/v1/sites/site-0001/networks/network-001" in requests
      assert "GET /proxy/network/integration/v1/sites/site-0001/wifi/broadcasts/wifi-001" in requests

      assert writes() == [], f"diff mode issued writes: {writes()}"

      # ---------------------------------------------------------------- drift
      # Change the console out from under the instance file: the DNS record now
      # points somewhere else, and mDNS forwarding has been turned off.
      machine.succeed(
          "${pkgs.jq}/bin/jq '.dns[0].ipv4Address = \"10.0.0.99\" "
          "| .networks[0].mdnsForwardingEnabled = false' "
          "/var/lib/fake-console/state.json > /tmp/drifted.json"
      )
      machine.succeed("mv /tmp/drifted.json /var/lib/fake-console/state.json")
      machine.succeed("truncate -s 0 /var/lib/fake-console/requests.log")

      # `unifi diff` exits 2 when the plan is non-empty, so the unit fails and
      # the drift is visible in systemctl status rather than buried.
      machine.fail("systemctl start unifi-sync.service")
      # `systemctl is-failed` exits 0 only when the unit is in the failed state,
      # so this asserts the unit was left failed and not merely that the start
      # command returned non-zero.
      assert machine.succeed("systemctl is-failed unifi-sync.service").strip() == "failed"

      # The plan naming what drifted is in the journal, which is the whole point
      # of failing rather than just returning non-zero.
      plan = machine.succeed("journalctl -u unifi-sync.service --no-pager | tail -n 40")
      assert "UPDATE network        Default (mdnsForwardingEnabled)" in plan, plan
      assert "UPDATE dns policy     A_RECORD host.test.invalid" in plan, plan

      # Still read-only. This is the assertion that matters most: the tool found
      # work to do and did not do any of it.
      assert writes() == [], f"diff mode issued writes on drift: {writes()}"

      # ------------------------------------------------------------ unifi-plan
      # The human-facing wrapper runs the same pipeline as `sync --dry-run`, and
      # is likewise read-only. Nothing is handed to it: it has to read the API
      # key from the file directly rather than from a systemd credential, and
      # load the SSID passphrase out of the same secrets file the unit gets as an
      # EnvironmentFile. The clean `OK wifi` line below is what proves it did —
      # without the passphrase that line would read UPDATE.
      plan = machine.succeed("unifi-plan")
      assert "DRY RUN" in plan, plan
      assert "OK     wifi           test-ssid" in plan, plan
      assert "UPDATE dns policy     A_RECORD host.test.invalid" in plan, plan
      assert writes() == [], f"unifi-plan issued writes: {writes()}"

      # ------------------------------------------------------------ zone rule
      # The fake enforces the console's rule directly: a network create without
      # a zone id is refused with the code a live console answers.
      machine.succeed("install -m 0644 ${fixtures} /var/lib/fake-console/state.json")
      refused = machine.succeed(
          "${pkgs.curl}/bin/curl -s -X POST -H 'Content-Type: application/json' "
          "--data-binary @${zonelessNetwork} "
          "http://127.0.0.1:8088/proxy/network/integration/v1/sites/site-0001/networks"
      )
      assert "api.network.validation.missing-zone-id" in refused, refused

      # A real sync creating a network in a zone that does not exist yet: the
      # zone first, then the network carrying its id.
      machine.succeed("truncate -s 0 /var/lib/fake-console/requests.log")
      env = "UNIFI_URL=http://127.0.0.1:8088 UNIFI_SITE=Default UNIFI_API_KEY=test-api-key"
      out = machine.succeed(f"{env} ${unifi} sync < ${labSite}")
      assert "CREATE firewall zone  Lab (1 networks)" in out, out
      assert "CREATE network        Lab (vlan 30, zone Lab)" in out, out
      assert writes() == [
          "POST /proxy/network/integration/v1/sites/site-0001/firewall/zones",
          "POST /proxy/network/integration/v1/sites/site-0001/networks",
      ], writes()

      import json
      st = json.loads(machine.succeed("cat /var/lib/fake-console/state.json"))
      lab_net = next(n for n in st["networks"] if n["name"] == "Lab")
      lab_zone = next(z for z in st["zones"] if z["name"] == "Lab")
      assert lab_net["zoneId"] == lab_zone["id"], st
      assert lab_zone["networkIds"] == [lab_net["id"]], st

      # And the result is converged: a second diff has nothing to do.
      machine.succeed(f"{env} ${unifi} diff < ${labSite}")

      # ------------------------------------------------------ protocol filter
      # The fake refuses TCP_UDP spelt as a NAMED_PROTOCOL, with the code a
      # live console answers.
      refused = machine.succeed(
          "${pkgs.curl}/bin/curl -s -X POST -H 'Content-Type: application/json' "
          "--data-binary @${namedTcpUdpPolicy} "
          "http://127.0.0.1:8088/proxy/network/integration/v1/sites/site-0001/firewall/policies"
      )
      assert "api.request.unknown-type-id" in refused, refused

      # A real sync sends TCP_UDP as the PRESET the console accepts, and reads
      # it back as TCP_UDP; the fake stores the DNS policy's lists reversed. The
      # second diff is still clean.
      machine.succeed("truncate -s 0 /var/lib/fake-console/requests.log")
      out = machine.succeed(f"{env} ${unifi} sync < ${policySite}")
      assert "CREATE firewall policy Internal -> Gateway / DNS" in out, out
      assert "CREATE firewall policy Internal -> Gateway / NTP" in out, out
      assert writes() == [
          "POST /proxy/network/integration/v1/sites/site-0001/firewall/policies",
          "POST /proxy/network/integration/v1/sites/site-0001/firewall/policies",
      ], writes()
      st = json.loads(machine.succeed("cat /var/lib/fake-console/state.json"))
      filters = {p["name"]: p["ipProtocolScope"]["protocolFilter"] for p in st["policies"]}
      assert filters["DNS"] == {"type": "PRESET", "preset": {"name": "TCP_UDP"}}, filters
      assert filters["NTP"]["type"] == "NAMED_PROTOCOL", filters
      dns = next(p for p in st["policies"] if p["name"] == "DNS")
      ports = [i["value"] for i in dns["destination"]["trafficFilter"]["portFilter"]["items"]]
      assert ports == [853, 53], ports
      machine.succeed(f"{env} ${unifi} diff < ${policySite}")
    '';
}
