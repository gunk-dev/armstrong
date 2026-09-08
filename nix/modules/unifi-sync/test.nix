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

  testScript = ''
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
  '';
}
