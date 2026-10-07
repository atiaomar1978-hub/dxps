"""Assemble the DxPS delivery folder: source archives, documents, test evidence and checksums."""
import hashlib
import io
import os
import shutil
import sys
import tarfile
import zipfile
from pathlib import Path

DATE = "2026-10-06"
ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "dxps"
DESIGN = ROOT / "design"
RESULTS = Path(os.environ.get("DXPS_RUNTIME", Path.home() / "dxps-runtime")) / "results"
OUT = ROOT / f"DxPS_Delivery_{DATE}"

EXCLUDE_DIRS = {"bin", ".git", ".idea", ".vscode", "node_modules", "__pycache__"}
EXCLUDE_EXT = {".exe", ".test", ".out", ".prof", ".key", ".pem", ".p12", ".crt", ".log", ".jsonl"}
EXCLUDE_NAMES = {"secrets.env", ".DS_Store"}

DOCS = [
    "DxPS_TO-BE_HLD_LLD.docx", "DxPS_TO-BE_HLD_LLD.pdf",
    "DxPS_Test_Cases_and_Results.docx", "DxPS_Test_Cases_and_Results.pdf",
    "DxPS_Installation_and_Running_Guide.docx", "DxPS_Installation_and_Running_Guide.pdf",
]
EVIDENCE = [
    "tests.json", "tests-raw.jsonl", "coverage.out", "probes.json", "gosec.json", "govulncheck.txt",
    "exchanges.json", "load-2000.json", "snapshot-after-load.json",
]
GENERATORS = [
    "build_dxps_tobe_docx.py", "build_dxps_testcases_docx.py", "build_dxps_install_guide.py", "package_delivery.py",
]


def source_files():
    for dirpath, dirnames, filenames in os.walk(SRC):
        dirnames[:] = sorted(d for d in dirnames if d not in EXCLUDE_DIRS)
        for name in sorted(filenames):
            p = Path(dirpath) / name
            if name in EXCLUDE_NAMES or p.suffix.lower() in EXCLUDE_EXT or name.startswith("secrets"):
                continue
            yield p, "dxps/" + p.relative_to(SRC).as_posix()


def normalized(p: Path) -> bytes:
    data = p.read_bytes()
    if p.suffix == ".sh":
        data = data.replace(b"\r\n", b"\n")
    return data


def build_zip(dest: Path, files):
    with zipfile.ZipFile(dest, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for p, arc in files:
            info = zipfile.ZipInfo(arc, date_time=(2026, 10, 6, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (0o100755 if p.suffix == ".sh" else 0o100644) << 16
            z.writestr(info, normalized(p))


def build_tgz(dest: Path, files):
    with tarfile.open(dest, "w:gz", compresslevel=9) as t:
        dirs = set()
        for _, arc in files:
            parts = arc.split("/")[:-1]
            for i in range(1, len(parts) + 1):
                dirs.add("/".join(parts[:i]))
        for d in sorted(dirs):
            ti = tarfile.TarInfo(d)
            ti.type, ti.mode, ti.mtime = tarfile.DIRTYPE, 0o755, 1791244800
            t.addfile(ti)
        for p, arc in files:
            data = normalized(p)
            ti = tarfile.TarInfo(arc)
            ti.size, ti.mtime = len(data), 1791244800
            ti.mode = 0o755 if p.suffix == ".sh" else 0o644
            t.addfile(ti, io.BytesIO(data))


def copy_evidence(dest: Path):
    dest.mkdir(parents=True)
    for name in EVIDENCE:
        src = RESULTS / name
        if not src.exists():
            print(f"warning: missing evidence {src}", file=sys.stderr)
            continue
        if name == "govulncheck.txt":
            raw = src.read_bytes()
            text = raw.decode("utf-16") if raw[:2] in (b"\xff\xfe", b"\xfe\xff") else raw.decode("utf-8-sig")
            (dest / name).write_text(text.replace("\r\n", "\n"), encoding="utf-8", newline="\n")
        else:
            shutil.copy2(src, dest / name)
    if (RESULTS / "payloads").is_dir():
        shutil.copytree(RESULTS / "payloads", dest / "payloads")


README = """DxPS - Real-time Provisioning System - delivery {date}
=========================================================

Contents
  dxps-source-{date}.zip       full source (Go module, scripts, migrations, tests) - use on Windows
  dxps-source-{date}.tar.gz    same source, Unix permissions preserved            - use on Ubuntu / macOS
  docs/                        TO-BE HLD+LLD, Test Cases and Results, Installation and Running Guide (docx + pdf)
  evidence/                    raw test run results: tests.json, coverage, live probes, security scans,
                               network-simulator exchanges, E2E payloads, 2,000-order load run
  generators/                  Python scripts that regenerate the Word documents from the evidence
  SHA256SUMS.txt               checksums of every file in this folder

Quick start (details: docs/DxPS_Installation_and_Running_Guide.pdf)

  Windows (PowerShell)
    Expand-Archive dxps-source-{date}.zip -DestinationPath $HOME ; cd $HOME\\dxps
    powershell -ExecutionPolicy Bypass -File scripts\\install-deps-windows.ps1
    powershell -ExecutionPolicy Bypass -File scripts\\infra-setup.ps1
    powershell -ExecutionPolicy Bypass -File scripts\\run-all.ps1

  Ubuntu 22.04 / 24.04 (normal user with sudo, not root)
    mkdir -p ~/dxps && tar -xzf dxps-source-{date}.tar.gz -C ~/dxps --strip-components=1 && cd ~/dxps
    bash scripts/install-deps-ubuntu.sh && bash scripts/infra-setup.sh && bash scripts/run-all.sh

  macOS 13+ (Homebrew)
    mkdir -p ~/dxps && tar -xzf dxps-source-{date}.tar.gz -C ~/dxps --strip-components=1 && cd ~/dxps
    bash scripts/install-deps-macos.sh && bash scripts/infra-setup.sh && bash scripts/run-all.sh

  Then open the Mosaic dashboard: http://127.0.0.1:8088
  Tests: scripts/test-all(.ps1|.sh)    Security scans: scripts/sec-scan(.ps1|.sh)    Stop: scripts/infra-stop

Security
  No secrets, keys or runtime data are shipped. infra-setup generates passwords, JWT/HMAC keys and a
  development CA locally (runtime folder, mode 0600). Replace the dev PKI with your own CA for any
  non-local deployment.

Verify the download
  Windows:  Get-FileHash -Algorithm SHA256 <file>
  Linux:    sha256sum -c SHA256SUMS.txt
  macOS:    shasum -a 256 -c SHA256SUMS.txt
"""


def main():
    if OUT.exists():
        shutil.rmtree(OUT)
    OUT.mkdir()
    files = list(source_files())
    build_zip(OUT / f"dxps-source-{DATE}.zip", files)
    build_tgz(OUT / f"dxps-source-{DATE}.tar.gz", files)

    (OUT / "docs").mkdir()
    for name in DOCS:
        shutil.copy2(ROOT / name, OUT / "docs" / name)
    copy_evidence(OUT / "evidence")
    (OUT / "generators").mkdir()
    for name in GENERATORS:
        shutil.copy2(DESIGN / name, OUT / "generators" / name)

    (OUT / "README.txt").write_text(README.format(date=DATE).replace("\n", "\r\n"), encoding="utf-8", newline="")

    lines = []
    for p in sorted(OUT.rglob("*")):
        if p.is_file():
            lines.append(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.relative_to(OUT).as_posix()}")
    (OUT / "SHA256SUMS.txt").write_text("\n".join(lines) + "\n", encoding="utf-8", newline="\n")

    print(f"{OUT}\n  source files: {len(files)}")
    for p in sorted(OUT.rglob("*")):
        if p.is_file():
            print(f"  {p.stat().st_size:>10,}  {p.relative_to(OUT).as_posix()}")


if __name__ == "__main__":
    main()
