#!/usr/bin/env python3
"""Prove redaction works: mock upstream, rdx in the guest, no model calls.

Runs entirely offline. Verifies that
  * `zbox redact apply` writes rules and a hashed secret rule (no plaintext),
  * rdx replaces the legend values on the way out,
  * two identical requests produce byte-identical upstream bodies (prompt cache),
  * internal hostnames and secret values do not reach the upstream.
"""
import json
import os
import shutil
import socket
import subprocess
import tempfile
import time
from pathlib import Path

repo = Path(__file__).resolve().parent.parent
root = Path(tempfile.mkdtemp(prefix="zr-", dir="/tmp")).resolve()
state = root / "state"
project = root / "project"
project.mkdir()
report = Path(tempfile.mkdtemp(prefix="zbox-redact-report-", dir="/tmp"))
base = [str(repo / "bin/zbox"), "--state-dir", str(state), "--vm", "redact-test"]
log = (report / "commands.log").open("w")
print(f"Report: {report}\nTemporary state: {state}", flush=True)

SECRET = "sk-probe-" + "0123456789abcdef" * 2
LEGEND_NAME = "Zed Proxy"


def agents(*args, timeout=120, check=True):
    command = base + list(args)
    log.write(f"\nCOMMAND {command!r}\n")
    log.flush()
    r = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                       text=True, errors="replace", timeout=timeout)
    log.write(r.stdout)
    log.flush()
    if check and r.returncode:
        raise RuntimeError(f"{args!r} failed:\n{r.stdout[-4000:]}")
    return r.stdout


(project / "config.env").write_text(f"PROBE_API_KEY={SECRET}\n")
(project / "README.md").write_text(
    "Internal notes: contact dev@example.com, host build-01.corp.\n")

# Mock upstream: records every request body it receives.
(project / "upstream.py").write_text('''import http.server, json, pathlib
store = pathlib.Path(__file__).with_name("upstream.log")
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n).decode()
        with store.open("a") as f:
            f.write(body + "\\n")
        payload = {"id": "msg_mock", "type": "message", "role": "assistant",
                   "content": [{"type": "text", "text": "ok"}], "model": "mock",
                   "stop_reason": "end_turn",
                   "usage": {"input_tokens": 1, "output_tokens": 1}}
        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def log_message(self, *args):
        pass
http.server.HTTPServer(("127.0.0.1", 19099), H).serve_forever()
''')


def guest_probe(port, body, req_path):
    req = project / "req.json"
    req.write_text(json.dumps(body))
    return agents("ssh", "curl", "-sS", "--fail", "--max-time", "20",
                  "-X", "POST", f"http://127.0.0.1:{port}/v1/messages",
                  "-H", "content-type: application/json",
                  "-H", "anthropic-version: 2023-06-01",
                  "-H", "x-api-key: placeholder",
                  "--data", f"@{req_path}")


try:
    agents("create", "--cpus", "2", "--memory", "2", "--disk", "10", timeout=180)
    agents("mounts", "add", str(project))
    print("Installing harnesses and rdx...", flush=True)
    agents("start", timeout=1200)
    home = agents("ssh", "sh", "-c", "echo $HOME").strip()
    legend = json.loads((state / "vms" / "redact-test" / "config.json").read_text())["persona"]
    username = legend["username"]
    guest_project = f"{home}/dev/{project.name}"
    tool = f"{home}/.local/share/uv/tools/claude-code-redact/bin/python"
    agents("ssh", "sh", "-c", f"test -x {tool}")

    print("Applying redaction rules...", flush=True)
    findings = [
        {"value": socket.gethostname(), "category": "HOST", "context": "test host"},
        {"value": "build-01.corp", "category": "HOST", "context": "README.md:1"},
        {"value": os.environ.get("USER", "dev"), "category": "NAME", "context": "test user"},
    ]
    findings_file = root / "findings.json"
    findings_file.write_text(json.dumps(findings))
    agents("redact", "apply", "--file", str(findings_file), "--yes", timeout=600)

    rules = (project / ".redaction_rules").read_text()
    assert "build-01.corp" in rules.replace("\\.", "."), rules
    assert SECRET not in rules, "secret plaintext leaked into the rules file"
    assert "hashed: true" in rules, rules
    print("PASS rules generated, secret stored as a hash only", flush=True)

    # Two identical requests must produce identical upstream bodies.
    prompt = (f"Primary working directory: {guest_project}\n"
              f"I am {os.environ.get('USER', 'dev')} on build-01.corp, "
              f"my key is {SECRET}\n")
    body = {"model": "claude-sonnet-4-5", "max_tokens": 16,
            "system": prompt,
            "messages": [{"role": "user", "content": prompt}]}

    agents("ssh", "sh", "-c",
           f"cd {guest_project} && setsid nohup {tool} upstream.py "
           f">/tmp/upstream.log 2>&1 < /dev/null & sleep 1")
    agents("ssh", "sh", "-c",
           f"cd {guest_project} && RDX_UPSTREAM_URL=http://127.0.0.1:19099 "
           f"setsid nohup {tool} -m uvicorn rdx.proxy.server:app "
           f"--host 127.0.0.1 --port 18643 >/tmp/rdx.log 2>&1 < /dev/null & sleep 3")
    wait = time.time() + 30
    while time.time() < wait:
        if agents("ssh", "curl", "-sS", "--max-time", "2",
                  "http://127.0.0.1:18643/health", check=False).find("ok") >= 0:
            break
        time.sleep(1)
    req_guest = f"{guest_project}/req.json"
    guest_probe(18643, body, req_guest)
    guest_probe(18643, body, req_guest)
    time.sleep(1)
    seen = (project / "upstream.log").read_text()
    bodies = [line for line in seen.splitlines() if line.strip()]
    assert len(bodies) == 2, f"expected two upstream requests, got {len(bodies)}"
    assert bodies[0] == bodies[1], "identical requests produced different upstream bodies"
    assert SECRET not in seen, "secret reached the upstream"
    assert "build-01.corp" not in seen, "internal hostname reached the upstream"
    assert os.environ.get("USER", "dev") not in seen.replace(username, ""), "host user reached the upstream"
    assert "__rdx_key_" in seen.lower() or "<redacted" in seen.lower(), seen[:2000]
    print("PASS upstream saw tokens only; identical requests are byte-identical", flush=True)

    (report / "result.txt").write_text(
        "PASS: rules generated, secret hashed, host identity and internal host "
        "redacted, prompt-cache-stable request bodies.\n")
finally:
    for path, label in ((project / "upstream.log", "upstream"), (report / "rdx.log", "rdx")):
        try:
            out = path.read_text()
        except OSError:
            out = ""
        if label == "rdx":
            try:
                out = agents("ssh", "cat", "/tmp/rdx.log", check=False, timeout=60)
            except Exception:
                pass
        if out:
            (report / f"{label}.log").write_text(out)
            print(f"--- {label}.log\n{out[-2000:]}", flush=True)
    print("Removing temporary VM...", flush=True)
    try:
        agents("delete", timeout=180, check=False)
        shutil.rmtree(root)
        print("Temporary VM removed.", flush=True)
    finally:
        log.close()
