#!/usr/bin/env python3
"""Render documentation SVGs with Mermaid CLI (mmdc); no VM is needed."""
import os
from pathlib import Path
import re
import subprocess
import xml.etree.ElementTree as ET

repo = Path(__file__).resolve().parent.parent
diagrams = repo / "docs/diagrams"
env = os.environ.copy()
chrome = Path("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
if not env.get("PUPPETEER_EXECUTABLE_PATH") and chrome.is_file():
    env["PUPPETEER_EXECUTABLE_PATH"] = str(chrome)

for source in sorted(diagrams.glob("*.mmd")):
    output = source.with_suffix(".svg")
    subprocess.run([
        "mmdc", "-i", str(source), "-o", str(output),
        "-c", str(diagrams / "mermaid.json"), "-b", "white", "-w", "1600",
    ], env=env, check=True)
    svg = output.read_text()
    root = ET.fromstring(svg)
    x, y, width, height = root.attrib["viewBox"].split()
    # Preserve Mermaid's word-leading tspan spaces in non-browser SVG viewers.
    # An explicit background also works in viewers that ignore CSS backgrounds.
    svg = svg.replace("<svg ", '<svg xml:space="preserve" ', 1)
    background = (f'<rect x="{x}" y="{y}" width="{width}" height="{height}" '
                  'fill="white" aria-hidden="true"/>')
    svg = re.sub(r"(<svg\b[^>]*>)", lambda m: m[0] + background, svg, count=1)
    output.write_text(svg + "\n")
    print(output.relative_to(repo))
