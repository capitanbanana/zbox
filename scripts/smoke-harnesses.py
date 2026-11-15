#!/usr/bin/env python3
"""Provision Pi/Ori, verify MCP and migrate a running VM; no model calls or login."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

repo = Path(__file__).resolve().parent.parent
root = Path(tempfile.mkdtemp(prefix="ah-", dir="/tmp")).resolve()
state = root / "state"
project = root / "project"
project.mkdir()
report = Path(tempfile.mkdtemp(prefix="zbox-harness-report-", dir="/tmp"))
base = [str(repo / "bin/zbox"), "--state-dir", str(state), "--vm", "harness-test"]
log = (report / "commands.log").open("w")
print(f"Report: {report}\nTemporary state: {state}", flush=True)


def agents(*args, timeout=120):
    command = base + list(args)
    log.write(f"\nCOMMAND {command!r}\n")
    log.flush()
    r = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                       text=True, timeout=timeout)
    log.write(r.stdout)
    log.flush()
    if r.returncode:
        raise RuntimeError(f"{args!r} failed:\n{r.stdout[-4000:]}")
    return r.stdout


(project / "mcp.py").write_text('''import json, os, pathlib, sys
pathlib.Path(__file__).with_name("observed.json").write_text(json.dumps({
    "cwd":os.getcwd(),
    "env":{k:os.environ.get(k) for k in ["HTTP_PROXY","HTTPS_PROXY","ALL_PROXY","PI_CODING_AGENT_DIR","CODEX_HOME"]}
}))
for line in sys.stdin:
    message=json.loads(line)
    if "id" not in message: continue
    method=message.get("method")
    if method=="initialize":
        result={"protocolVersion":message["params"]["protocolVersion"],"capabilities":{"tools":{}},"serverInfo":{"name":"harness-smoke","version":"1"}}
    elif method=="tools/list": result={"tools":[]}
    else: result={}
    print(json.dumps({"jsonrpc":"2.0","id":message["id"],"result":result}),flush=True)
''')
try:
    agents("create", "--cpus", "2", "--memory", "2", "--disk", "10", timeout=180)
    agents("mounts", "add", str(project))
    print("Installing default guest harnesses...", flush=True)
    agents("start", timeout=900)
    home = agents("ssh", "sh", "-c", "echo $HOME").strip()
    guest_project = f"{home}/dev/{project.name}"
    for harness in ["claude", "codex", "pi", "ori", "openrouter"]:
        output = agents(harness, str(project), "--version")
        print(f"PASS {harness} --version: {output.strip().splitlines()[-1]}", flush=True)
    agents("pi", str(project), "--help")
    agents("ori", str(project), "--help")
    agents("ori", str(project), "login", "--help")
    agents("mcp", "add", "probe", "--", "python3", str(Path(guest_project) / "mcp.py"))
    output = agents("pi", str(project), "mcp", "list", "--json")
    assert "probe" in output, output
    seen = json.loads((project / "observed.json").read_text())
    assert seen["cwd"] == guest_project, seen
    assert seen["env"]["PI_CODING_AGENT_DIR"] == f"{home}/.config/agents/pi", seen
    assert seen["env"]["HTTPS_PROXY"] == "http://127.0.0.1:18080", seen
    print("PASS Pi native MCP, mounted working directory and guest proxy environment", flush=True)
    agents("mcp", "disable", "probe")
    (project / "observed.json").unlink()
    agents("pi", str(project), "mcp", "list", "--json")
    assert not (project / "observed.json").exists(), "disabled MCP started"
    print("PASS managed MCP removal applies on next Pi session", flush=True)
    before = agents("ssh", "cat", "/proc/sys/kernel/random/boot_id").strip()
    # Simulate a pre-Pi/Ori installation, retaining the already provisioned disk.
    agents("ssh", "sh", "-c",
           'mv "$HOME/.local/bin/pi" "$HOME/.local/bin/pi.saved"; '
           'mv "$HOME/.local/bin/ori" "$HOME/.local/bin/ori.saved"; '
           'printf preserve > "$HOME/.config/agents/pi/state-sentinel"')
    agents("start", timeout=600)
    after = agents("ssh", "cat", "/proc/sys/kernel/random/boot_id").strip()
    assert before == after, "migration restarted VM"
    for harness in ["pi", "ori"]:
        agents(harness, str(project), "--version")
    assert agents("ssh", "cat", f"{home}/.config/agents/pi/state-sentinel").strip() == "preserve"
    print("PASS missing Pi/Ori installed into running VM without reboot or state loss", flush=True)
    (report / "result.txt").write_text("PASS: four installed tools and OpenRouter alias; Pi MCP/cwd/proxy; disabled MCP; running-VM migration. No login or model calls performed.\n")
finally:
    print("Removing temporary VM...", flush=True)
    try:
        agents("delete", timeout=120)
        shutil.rmtree(root)
        print("Temporary VM removed.", flush=True)
    finally:
        log.close()
