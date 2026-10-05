#!/usr/bin/env python3
"""Exercise an image with disposable data and a local mock LLM (no API keys).

Run: python3 scripts/docker-smoke.py agenticgo:local [--require-sandbox]
Requires a local Docker daemon. --require-sandbox also requires the host/runtime
to permit bubblewrap's namespaces; otherwise that limitation is reported.
"""

import argparse
import http.server
import json
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid


class MockLLM(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        last = body["messages"][-1]
        if last["role"] == "tool":
            delta = {"content": last["content"]}
            finish = "stop"
        else:
            tool, args = {
                "write": ("write_file", {"path": "probe.txt", "content": "persisted marker"}),
                "read": ("read_file", {"path": "probe.txt"}),
                "escape": ("read_file", {"path": "../../secret.key"}),
                "standard": ("exec", {"command": "pwd"}),
                "extra": ("exec", {"command": "curl --version"}),
            }[last["content"]]
            delta = {"tool_calls": [{
                "index": 0, "id": "smoke-call", "type": "function",
                "function": {"name": tool, "arguments": json.dumps(args)},
            }]}
            finish = "tool_calls"
        chunk = {
            "id": "chatcmpl-smoke", "object": "chat.completion.chunk",
            "created": 1, "model": "smoke",
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
        }
        data = ("data: " + json.dumps(chunk) + "\n\ndata: [DONE]\n\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def docker(*args, check=True):
    result = subprocess.run(["docker", *args], text=True, capture_output=True, timeout=90)
    if check and result.returncode:
        raise RuntimeError(f"docker {' '.join(args)}: {result.stderr.strip()}")
    return result.stdout.strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image", nargs="?", default="agenticgo:local")
    parser.add_argument("--require-sandbox", action="store_true")
    args = parser.parse_args()
    name = "agenticgo-smoke-" + uuid.uuid4().hex[:10]
    volume = name + "-data"
    mock = http.server.ThreadingHTTPServer(("0.0.0.0", 0), MockLLM)
    thread = threading.Thread(target=mock.serve_forever, daemon=True)
    thread.start()
    base = ""

    def api(path, body=None, method=None):
        request = urllib.request.Request(
            base + path,
            data=None if body is None else json.dumps(body).encode(),
            headers={"Content-Type": "application/json"}, method=method,
        )
        with urllib.request.urlopen(request, timeout=40) as response:
            return json.load(response)

    def start():
        nonlocal base
        docker("run", "-d", "--name", name, "--stop-timeout", "30",
               "--add-host", "host.docker.internal:host-gateway",
               "-p", "127.0.0.1::8080", "-v", volume + ":/data",
               "-e", "AGENTICGO_EXTRA_EXEC_COMMANDS=curl", args.image)
        info = json.loads(docker("inspect", name))[0]
        port = info["NetworkSettings"]["Ports"]["8080/tcp"][0]["HostPort"]
        base = "http://127.0.0.1:" + port
        deadline = time.monotonic() + 30
        while True:
            try:
                assert api("/healthz")["status"] == "ok"
                break
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                if time.monotonic() >= deadline:
                    raise RuntimeError("container did not become ready: " + docker("logs", name))
                time.sleep(0.2)
        healthcheck = info["Config"]["Healthcheck"]["Test"]
        docker("exec", name, "/bin/sh", "-c", healthcheck[1])

    def chat(message):
        return api("/api/chat", {"agent": "smoke", "session": "smoke", "message": message})["reply"]

    try:
        docker("volume", "create", volume)
        start()
        assert docker("exec", name, "id", "-u") == "10001"
        api("/api/providers", {
            "name": "mock", "base_url": f"http://host.docker.internal:{mock.server_port}/v1",
            "model": "smoke", "api_key": "smoke-key", "default": True,
        })
        api("/api/agents", {"key": "smoke", "name": "Smoke"})
        assert "wrote" in chat("write")
        assert chat("read") == "persisted marker"
        assert "error:" in chat("escape")
        assert "not on the exec allow-list" in chat("extra")
        api("/api/agents/smoke/extra-commands/curl", method="PUT")
        assert "curl " in chat("extra")
        print("PASS: health check, non-root user, workspace tools, escape rejection, extra-command gating")

        standard = chat("standard").strip()
        sandbox_ok = standard == "/workspace"
        if sandbox_ok:
            print("PASS: standard exec runs inside the bubblewrap workspace")
        else:
            assert standard.startswith("error:"), standard
            print("SANDBOX UNAVAILABLE under this Docker runtime:", standard)

        stored = docker("exec", name, "cat", "/data/providers.json")
        assert "smoke-key" not in stored and "enc:v1:" in stored
        key_before = docker("exec", name, "sha256sum", "/data/secret.key")
        docker("stop", name)
        assert json.loads(docker("inspect", name))[0]["State"]["ExitCode"] == 0
        docker("rm", name)
        start()
        assert docker("exec", name, "sha256sum", "/data/secret.key") == key_before
        assert api("/api/providers/mock")["api_key"] == "smoke-key"
        assert chat("read") == "persisted marker"
        assert len(api("/api/sessions/smoke/smoke/messages")) >= 20
        print("PASS: encrypted credentials, graceful shutdown, data/history across container recreation")
        if args.require_sandbox and not sandbox_ok:
            raise RuntimeError("--require-sandbox: configure the Docker runtime to allow bubblewrap namespaces")
    finally:
        docker("rm", "-f", name, check=False)
        docker("volume", "rm", volume, check=False)
        mock.shutdown()
        mock.server_close()
        thread.join()


if __name__ == "__main__":
    main()
