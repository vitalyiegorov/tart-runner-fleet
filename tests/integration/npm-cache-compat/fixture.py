#!/usr/bin/env python3
"""Local stand-ins for the npm cache compatibility run (ADR 0092).

`registry PORT LOG` serves a registry whose packages exist to make a client's
supply-chain check fire. It logs every request's method, path and whether it
carried an Authorization header, never the header's value.

`record PORT TARGET LOG` is a transparent HTTP proxy in front of the gate. It logs
every request and header it passes, which is how the run proves what a guest does
and does not send to the gate.
"""
import base64
import gzip
import hashlib
import http.client
import http.server
import io
import json
import sys
import tarfile
from datetime import datetime, timedelta, timezone

OLD = "2020-01-01T00:00:00.000Z"
NOW = datetime.now(timezone.utc)


def tarball(name, version):
    """A minimal valid package tarball, identical for identical arguments."""
    manifest = json.dumps({"name": name, "version": version}).encode()
    out = io.BytesIO()
    # gzip's header carries a clock unless told not to, and integrity is over the bytes.
    with gzip.GzipFile(fileobj=out, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            info = tarfile.TarInfo("package/package.json")
            info.size = len(manifest)
            archive.addfile(info, io.BytesIO(manifest))
    return out.getvalue()


def integrity(data):
    return "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode()


def version_doc(port, name, version, extra=None, integrity_of=None):
    data = tarball(name, version)
    base = name.rsplit("/", 1)[-1]
    doc = {
        "name": name,
        "version": version,
        "dist": {
            "tarball": f"http://127.0.0.1:{port}/{name}/-/{base}-{version}.tgz",
            "shasum": hashlib.sha1(data).hexdigest(),  # noqa: S324 - the registry field.
            "integrity": integrity(integrity_of if integrity_of is not None else data),
        },
    }
    for key, value in (extra or {}).items():
        if key == "dist":
            doc["dist"].update(value)
        else:
            doc[key] = value
    return doc


def packuments(port):
    trusted = {
        "_npmUser": {"name": "fixture", "email": "fixture@example.invalid",
                     "trustedPublisher": {"id": "github", "oidcConfigId": "oidc:fixture"}},
        "dist": {"attestations": {"url": "https://example.invalid/attestations",
                                  "provenance": {"predicateType": "https://slsa.dev/provenance/v1"}}},
    }
    young = (NOW - timedelta(minutes=1)).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    older = (NOW - timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    return {
        # 1.0.0 was published by a trusted publisher, and 1.0.1 was not: a downgrade.
        "trust-fixture": {
            "name": "trust-fixture", "dist-tags": {"latest": "1.0.1"},
            "time": {"created": OLD, "modified": older, "1.0.0": OLD, "1.0.1": older},
            "versions": {"1.0.0": version_doc(port, "trust-fixture", "1.0.0", trusted),
                         "1.0.1": version_doc(port, "trust-fixture", "1.0.1")},
        },
        # Published a minute ago, and nothing older exists to fall back to.
        "young-fixture": {
            "name": "young-fixture", "dist-tags": {"latest": "1.0.0"},
            "time": {"created": young, "modified": young, "1.0.0": young},
            "versions": {"1.0.0": version_doc(port, "young-fixture", "1.0.0")},
        },
        # What is advertised and what is served differ: a tampered tarball.
        "tamper-fixture": {
            "name": "tamper-fixture", "dist-tags": {"latest": "1.0.0"},
            "time": {"created": OLD, "modified": OLD, "1.0.0": OLD},
            "versions": {"1.0.0": version_doc(port, "tamper-fixture", "1.0.0", integrity_of=b"not these bytes")},
        },
        # A private package: only the private registry stand-in has it.
        "@private/pkg": {
            "name": "@private/pkg", "dist-tags": {"latest": "1.0.0"},
            "time": {"created": OLD, "modified": OLD, "1.0.0": OLD},
            "versions": {"1.0.0": version_doc(port, "@private/pkg", "1.0.0")},
        },
    }


class Registry(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    port = 0
    log_path = ""

    def log_message(self, *args):  # Quiet: the run reads the log file instead.
        pass

    def reply(self, status, body, content_type="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def do_GET(self):  # noqa: N802 - http.server's spelling.
        path = self.path.split("?")[0].replace("%2f", "/").replace("%2F", "/")
        with open(self.log_path, "a", encoding="utf-8") as log:
            log.write(json.dumps({"method": self.command, "path": path,
                                  "authorized": "Authorization" in self.headers}) + "\n")
        docs = packuments(self.port)
        name = path.lstrip("/")
        if name in docs:
            self.reply(200, json.dumps(docs[name]).encode())
            return
        if "/-/" in name:
            package, _, file = name.partition("/-/")
            version = file.removesuffix(".tgz").rsplit("-", 1)[-1]
            if package in docs and version in docs[package]["versions"]:
                self.reply(200, tarball(package, version), "application/octet-stream")
                return
        self.reply(404, b'{"error":"not found"}')

    do_HEAD = do_GET


def recorder(target_host, target_port, log_path):
    class Recorder(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def relay(self):
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else None
            with open(log_path, "a", encoding="utf-8") as log:
                log.write(json.dumps({"method": self.command, "path": self.path,
                                      "headers": dict(self.headers.items())}) + "\n")
            upstream = http.client.HTTPConnection(target_host, target_port, timeout=300)
            try:
                upstream.request(self.command, self.path, body=body, headers=dict(self.headers.items()))
                answer = upstream.getresponse()
                data = answer.read()
            except OSError:
                self.send_response(502)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            finally:
                upstream.close()
            self.send_response(answer.status)
            for key, value in answer.getheaders():
                if key.lower() not in ("transfer-encoding", "connection", "content-length"):
                    self.send_header(key, value)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(data)

        do_GET = do_HEAD = do_PUT = do_POST = do_DELETE = do_PATCH = relay

    return Recorder


def main():
    mode = sys.argv[1]
    port = int(sys.argv[2])
    if mode == "registry":
        Registry.port = port
        Registry.log_path = sys.argv[3]
        handler = Registry
    else:
        host, _, target_port = sys.argv[3].partition(":")
        handler = recorder(host, int(target_port), sys.argv[4])
    http.server.ThreadingHTTPServer(("127.0.0.1", port), handler).serve_forever()


if __name__ == "__main__":
    main()
