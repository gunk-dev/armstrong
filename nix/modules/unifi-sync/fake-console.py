"""A stand-in for the UniFi Network Integration API, for the unifi-sync VM
test.

It serves the read endpoints `unifi diff` actually walks, in the shapes
recorded in docs/unifi-api-notes.md and exercised by cmd/unifi's own fake:
offset/limit pages, list responses that are overviews rather than full
objects, and server-assigned ids and metadata.

Two things make it a useful test double rather than a mock:

  * the served state is a JSON file the test rewrites, so "the console drifted"
    is a real change to what the API returns, not a stubbed return value;
  * every request is appended to a log, including its method, so the test can
    assert that a diff-mode run issued no POST/PUT/DELETE.

The only writes it serves are creates of networks and firewall zones, with the
console's rule between them: while it has zones, `POST /networks` without the
id of one of them answers 400 `api.network.validation.missing-zone-id`, and a
created network joins that zone's `networkIds`. Every other write is answered
405 rather than being quietly accepted, so a tool that tried one would fail
loudly instead of looking like it had nothing to do.
"""

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PREFIX = "/proxy/network/integration/v1"
SITE_ID = "site-0001"

STATE = os.environ["FAKE_CONSOLE_STATE"]
REQUESTS = os.environ["FAKE_CONSOLE_REQUESTS"]
PORT = int(os.environ["FAKE_CONSOLE_PORT"])


def state():
    with open(STATE) as f:
        return json.load(f)


def save(st):
    with open(STATE + ".tmp", "w") as f:
        json.dump(st, f)
    os.replace(STATE + ".tmp", STATE)


def next_id(st, kind):
    """A fresh id in the fixture's `<kind>-NNN` style."""
    prefix = {"networks": "network", "zones": "zone"}[kind]
    taken = [int(o["id"].rsplit("-", 1)[1]) for o in st[kind] if o["id"].startswith(prefix + "-")]
    return "%s-%03d" % (prefix, max(taken, default=0) + 1)


def join_zone(st, zone_id, network_id):
    """Make a network a member of exactly one zone, keeping the network's
    zoneId and the zones' networkIds in step, as the console does."""
    for zone in st["zones"]:
        members = [n for n in zone["networkIds"] if n != network_id]
        if zone["id"] == zone_id:
            members.append(network_id)
        zone["networkIds"] = members
    for network in st["networks"]:
        if network["id"] == network_id:
            network["zoneId"] = zone_id


# The fields the real API omits from a list response. The tool has to follow up
# with a detail GET to see the rest, and this is what forces it to.
OVERVIEW = {
    "networks": ["id", "name", "management", "enabled", "vlanId", "default", "metadata"],
    "wifi": [
        "id",
        "type",
        "name",
        "enabled",
        "metadata",
        "network",
        "broadcastingFrequenciesGHz",
    ],
}


def overview(kind, obj):
    keep = OVERVIEW.get(kind)
    if keep is None:
        return obj
    out = {k: obj[k] for k in keep if k in obj}
    # The wifi overview carries the security type but never the passphrase.
    if kind == "wifi" and "securityConfiguration" in obj:
        out["securityConfiguration"] = {"type": obj["securityConfiguration"]["type"]}
    return out


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def record(self):
        with open(REQUESTS, "a") as f:
            f.write("%s %s\n" % (self.command, self.path.split("?")[0]))
            f.flush()

    def reply(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def page(self, items):
        self.reply(
            {
                "offset": 0,
                "limit": 200,
                "count": len(items),
                "totalCount": len(items),
                "data": items,
            }
        )

    def missing(self):
        self.reply({"statusCode": 404, "code": "api.not-found", "message": self.path}, 404)

    def do_GET(self):
        self.record()
        path = self.path.split("?")[0]
        if not path.startswith(PREFIX):
            return self.missing()
        rest = path[len(PREFIX) :]

        if rest == "/info":
            return self.reply({"applicationVersion": "10.6.101"})
        if rest == "/sites":
            return self.page([{"id": SITE_ID, "internalReference": "default", "name": "Default"}])

        prefix = "/sites/%s/" % SITE_ID
        if not rest.startswith(prefix):
            return self.missing()
        rest = rest[len(prefix) :]

        st = state()
        for coll, kind in (
            ("networks", "networks"),
            ("wifi/broadcasts", "wifi"),
            ("dns/policies", "dns"),
            ("firewall/zones", "zones"),
            ("firewall/policies", "policies"),
        ):
            items = st[kind]
            if rest == coll:
                return self.page([overview(kind, o) for o in items])
            if rest.startswith(coll + "/"):
                wanted = rest[len(coll) + 1 :]
                for obj in items:
                    if obj["id"] == wanted:
                        return self.reply(obj)
                return self.missing()
        return self.missing()

    def refuse(self):
        # Recorded first: the point of the log is to catch the attempt, whether
        # or not the console would have honoured it.
        self.record()
        self.reply(
            {"statusCode": 405, "code": "api.method-not-allowed", "message": self.command}, 405
        )

    def fail(self, code, message):
        self.reply(
            {"statusCode": 400, "statusName": "BAD_REQUEST", "code": code, "message": message}, 400
        )

    def do_POST(self):
        rest = self.path.split("?")[0][len(PREFIX + "/sites/%s/" % SITE_ID) :]
        if rest not in ("networks", "firewall/zones"):
            return self.refuse()
        self.record()
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        st = state()
        if rest == "networks":
            zone_id = body.get("zoneId")
            if st["zones"] and zone_id not in [z["id"] for z in st["zones"]]:
                return self.fail("api.network.validation.missing-zone-id", "zoneId must not be null")
            obj = dict(body, id=next_id(st, "networks"), default=False)
            obj["metadata"] = {"origin": "USER_DEFINED", "configurable": True}
            st["networks"].append(obj)
            if st["zones"]:
                join_zone(st, zone_id, obj["id"])
        else:
            obj = dict(body, id=next_id(st, "zones"))
            obj["metadata"] = {"origin": "USER_DEFINED", "configurable": True}
            st["zones"].append(obj)
            for network_id in list(obj.get("networkIds", [])):
                join_zone(st, obj["id"], network_id)
        save(st)
        self.reply(obj, 201)

    do_PUT = refuse
    do_DELETE = refuse
    do_PATCH = refuse


if __name__ == "__main__":
    open(REQUESTS, "a").close()
    server = HTTPServer(("127.0.0.1", PORT), Handler)
    print("fake console listening on %d" % PORT, file=sys.stderr, flush=True)
    server.serve_forever()
