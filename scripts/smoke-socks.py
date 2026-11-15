#!/usr/bin/env python3
"""Real macOS/Lima test. Downloads packages; uses no model or real VLESS account."""
import http.server
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tempfile
import threading
import time

repo = Path(__file__).resolve().parent.parent
state = Path(tempfile.mkdtemp(prefix="as-", dir="/tmp")).resolve()
report = Path(tempfile.mkdtemp(prefix="agents-socks-report-", dir="/tmp"))
base = [str(repo / "bin/agents"), "--state-dir", str(state), "--vm", "socks-test"]
log = (report / "commands.log").open("w")
tunnel = None
print(f"Report: {report}\nTemporary VM: {state}", flush=True)


def run(args, timeout=90, success=True):
    log.write(f"\nCOMMAND {args!r}\n")
    log.flush()
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            text=True, timeout=timeout)
    log.write(result.stdout)
    log.flush()
    if success and result.returncode:
        raise RuntimeError(f"Command failed: {args!r}\n{result.stdout[-3000:]}")
    return result


def agents(*args, **kw):
    return run(base + list(args), **kw)


def unused_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def listening(port):
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=.2):
            return True
    except OSError:
        return False


class HostHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"WRONG-HOST")

    def log_message(self, *args):
        pass


host = http.server.ThreadingHTTPServer(("127.0.0.1", 0), HostHandler)
threading.Thread(target=host.serve_forever, daemon=True).start()
# Reserve a port without listening: the test VLESS endpoint is always offline.
offline = socket.socket()
offline.bind(("127.0.0.1", 0))
port = unused_port()
proxy = f"socks5h://127.0.0.1:{port}"


def fetch(url, success=True):
    return run(["curl", "--noproxy", "", "--proxy", proxy, "--fail", "--silent",
                "--show-error", "--connect-timeout", "4", "--max-time", "15", url],
               success=success)


def close_tunnel():
    global tunnel
    if tunnel is not None and tunnel.poll() is None:
        os.killpg(tunnel.pid, signal.SIGINT)
        tunnel.wait(timeout=10)
    tunnel = None


try:
    agents("create", "--cpus", "2", "--memory", "2", "--disk", "10", timeout=180)
    uri = ("vless://11111111-2222-4333-8444-555555555555@127.0.0.1:"
           f"{offline.getsockname()[1]}?security=none")
    agents("vpn", "add", uri)
    agents("vpn", "bypass", "add", "test-lan")
    print("Provisioning VM through trusted LAN...", flush=True)
    agents("start", timeout=600)
    web_port = host.server_port
    agents("ssh", "sh", "-c",
           "mkdir -p /tmp/socks-web; printf VM-ONLY > /tmp/socks-web/index.html; "
           f"nohup python3 -m http.server {web_port} --bind 127.0.0.1 "
           "--directory /tmp/socks-web >/tmp/socks-web.log 2>&1 </dev/null &")
    tunnel = subprocess.Popen(base + ["socks", "--port", str(port)],
                              stdout=log, stderr=log, start_new_session=True)
    for _ in range(100):
        if listening(port):
            break
        if tunnel.poll() is not None:
            raise RuntimeError("SOCKS command exited before opening listener")
        time.sleep(.1)
    assert listening(port), "SOCKS listener never opened"
    local_url = f"http://127.0.0.1:{web_port}/"
    assert fetch(local_url).stdout == "VM-ONLY", "localhost reached Mac instead of VM"
    print("PASS host browser proxy reaches guest localhost, not Mac", flush=True)
    fetch("https://example.com")
    print("PASS remote hostname and HTTPS through guest proxy", flush=True)
    conflict = agents("socks", "--port", str(port), success=False, timeout=15)
    assert conflict.returncode and "forward" in conflict.stdout.lower(), conflict.stdout
    assert fetch(local_url).stdout == "VM-ONLY"
    print("PASS occupied port rejected without disrupting existing tunnel", flush=True)
    agents("vpn", "bypass", "delete", "test-lan")
    assert fetch("https://example.com", success=False).returncode, "VPN kill switch bypassed"
    assert fetch(local_url).stdout == "VM-ONLY"
    print("PASS VPN kill switch blocks external requests; guest localhost stays available", flush=True)
    agents("network", "add", "example.com")
    fetch("https://example.com")
    assert 'HOST "example.com:443"' in (state / "vms/socks-test/proxy.log").read_text()
    print("PASS live network exception routes browser through Mac with VPN down", flush=True)
    agents("network", "remove", "example.com")
    assert fetch("https://example.com", success=False).returncode, "removed exception still active"
    close_tunnel()
    for _ in range(30):
        if not listening(port):
            break
        time.sleep(.1)
    assert not listening(port), "Ctrl+C left SOCKS listener running"
    env = dict(os.environ, LIMA_HOME=str(state / "lima"))
    result = subprocess.run(["limactl", "list", "--json", "socks-test"],
                            env=env, capture_output=True, text=True, check=True)
    assert json.loads(result.stdout)["status"] == "Running", "Ctrl+C stopped VM"
    print("PASS Ctrl+C closes listener and leaves VM running", flush=True)
    (report / "result.txt").write_text("PASS: localhost, remote DNS/HTTPS, port conflict, VPN kill switch, live network exceptions, Ctrl+C lifecycle.\n")
finally:
    close_tunnel()
    host.shutdown()
    host.server_close()
    offline.close()
    print("Removing temporary VM...", flush=True)
    try:
        agents("delete", timeout=120)
        shutil.rmtree(state)
        print("Temporary VM removed.", flush=True)
    finally:
        log.close()
