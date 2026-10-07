"""Assemble the DxPS release: a Windows bundle, an Ubuntu/macOS bundle, the test evidence and checksums.

Both bundles contain the same tree under dxps/ (source, scripts, manuals, RELEASE.txt); they differ only in
line endings and file modes. The output folder is uploaded as-is to the GitHub release.
"""
import hashlib
import io
import os
import shutil
import sys
import tarfile
import zipfile
from pathlib import Path

VERSION = "1.0.0"
DATE = "2026-10-07"
MTIME = 1791331200  # 2026-10-07 00:00 UTC
ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "dxps"
DESIGN = ROOT / "design"
RESULTS = Path(os.environ.get("DXPS_RUNTIME", Path.home() / "dxps-runtime")) / "results"
OUT = ROOT / f"DxPS_Delivery_{DATE}"

EXCLUDE_DIRS = {"bin", ".git", ".idea", ".vscode", "node_modules", "__pycache__"}
EXCLUDE_EXT = {".exe", ".test", ".out", ".prof", ".key", ".pem", ".p12", ".crt", ".log", ".jsonl"}
EXCLUDE_NAMES = {"secrets.env", ".DS_Store"}

MANUALS = [
    "DxPS_TO-BE_HLD_LLD", "DxPS_Test_Cases_and_Results",
    "DxPS_Components_and_Data_Guide", "DxPS_Installation_and_Running_Guide",
]
EVIDENCE = [
    "tests.json", "tests-raw.jsonl", "coverage.out", "probes.json", "gosec.json", "govulncheck.txt",
    "exchanges.json", "load-2000.json", "snapshot-after-load.json",
]
GENERATORS = [
    "build_dxps_tobe_docx.py", "build_dxps_testcases_docx.py", "build_dxps_components_docx.py",
    "build_dxps_install_guide.py", "diagrams.py", "package_delivery.py",
]

RELEASE_COMMON = """DxPS - Real-time Provisioning System - release {version} ({date})
==================================================================

Contents of this folder
  cmd/, internal/         Go source of the 6 services and the dxpsctl CLI, with unit / integration / E2E tests
  scripts/                install, setup, run, status, test, security-scan and stop scripts
  docs/manuals/           TO-BE HLD+LLD, Test Cases and Results, Components and Data Guide,
                          Installation and Running Guide (docx + pdf)
  docs/COMPONENTS.md      components and data guide (Markdown)
  INSTALL.md              installation and running guide (Markdown)

Security
  No secrets, keys or runtime data are shipped. infra-setup generates passwords, JWT/HMAC keys and a
  development CA locally (runtime folder, mode 0600). Replace the dev PKI with your own CA for any
  non-local deployment.
"""

RELEASE_WINDOWS = RELEASE_COMMON + """
Quick start - Windows 10/11 (PowerShell, from this folder)
  Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
  .\\scripts\\install-deps-windows.ps1     # Go, Java 21, PostgreSQL 18, Kafka 4 (once)
  .\\scripts\\infra-setup.ps1              # secrets, dev PKI, database, Kafka storage (once)
  .\\scripts\\run-all.ps1                  # build, migrate, seed and start everything
  .\\scripts\\status.ps1

  Mosaic dashboard: http://127.0.0.1:8088
  Tests: .\\scripts\\test-all.ps1    Security scans: .\\scripts\\sec-scan.ps1    Stop: .\\scripts\\infra-stop.ps1
  Full guide: docs\\manuals\\DxPS_Installation_and_Running_Guide.pdf
"""

RELEASE_UNIX = RELEASE_COMMON + """
Quick start - Ubuntu 22.04 / 24.04 (normal user with sudo, not root)
  bash scripts/install-deps-ubuntu.sh       # Go, Java 21, PostgreSQL 18, Kafka 4 (once)
  bash scripts/infra-setup.sh               # secrets, dev PKI, database, Kafka storage (once)
  bash scripts/run-all.sh                   # build, migrate, seed and start everything
  bash scripts/status.sh

Quick start - macOS 13+ (Homebrew): the same, with scripts/install-deps-macos.sh as the first step.

  Mosaic dashboard: http://127.0.0.1:8088 (on a remote server: ssh -L 8088:127.0.0.1:8088 user@server)
  Tests: bash scripts/test-all.sh    Security scans: bash scripts/sec-scan.sh    Stop: bash scripts/infra-stop.sh
  Full guide: docs/manuals/DxPS_Installation_and_Running_Guide.pdf
"""

README = """DxPS - Real-time Provisioning System - delivery {version} ({date})
=================================================================

  dxps-{version}-windows.zip      Windows 10/11 bundle: source, scripts, manuals
  dxps-{version}-ubuntu.tar.gz    Ubuntu 22.04/24.04 and macOS bundle: same content, Unix line endings and modes
  dxps-{version}-evidence.zip     test evidence (tests, coverage, live probes, security scans, payloads, load run)
                                  and the scripts that regenerate the documents
  SHA256SUMS.txt                  checksums

Windows
  Expand-Archive dxps-{version}-windows.zip -DestinationPath C:\\ ; cd C:\\dxps ; notepad RELEASE.txt

Ubuntu / macOS
  mkdir -p ~/dxps && tar -xzf dxps-{version}-ubuntu.tar.gz -C ~/dxps --strip-components=1 && cd ~/dxps && cat RELEASE.txt

Verify the download
  Windows:  Get-FileHash -Algorithm SHA256 <file>
  Linux:    sha256sum -c SHA256SUMS.txt
  macOS:    shasum -a 256 -c SHA256SUMS.txt
"""


def source_files():
    for dirpath, dirnames, filenames in os.walk(SRC):
        dirnames[:] = sorted(d for d in dirnames if d not in EXCLUDE_DIRS)
        for name in sorted(filenames):
            p = Path(dirpath) / name
            if name in EXCLUDE_NAMES or p.suffix.lower() in EXCLUDE_EXT or name.startswith("secrets"):
                continue
            yield p, "dxps/" + p.relative_to(SRC).as_posix()


def bundle_entries(windows: bool):
    """(archive path, bytes, executable) for one platform bundle."""
    out = []
    for p, arc in source_files():
        data = p.read_bytes()
        if p.suffix in (".sh", ".go", ".sql", ".yaml", ".tmpl", ".md", ".mod", ".sum", ".js", ".css", ".html"):
            data = data.replace(b"\r\n", b"\n")
        if windows and p.suffix == ".ps1":
            data = data.replace(b"\r\n", b"\n").replace(b"\n", b"\r\n")
        out.append((arc, data, p.suffix == ".sh"))
    for m in MANUALS:
        for ext in (".pdf", ".docx"):
            out.append((f"dxps/docs/manuals/{m}{ext}", (ROOT / (m + ext)).read_bytes(), False))
    text = (RELEASE_WINDOWS if windows else RELEASE_UNIX).format(version=VERSION, date=DATE)
    if windows:
        text = text.replace("\n", "\r\n")
    out.append(("dxps/RELEASE.txt", text.encode("utf-8"), False))
    return sorted(out)


def build_zip(dest: Path, entries):
    with zipfile.ZipFile(dest, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for arc, data, exe in entries:
            info = zipfile.ZipInfo(arc, date_time=(2026, 10, 7, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (0o100755 if exe else 0o100644) << 16
            z.writestr(info, data)


def build_tgz(dest: Path, entries):
    with tarfile.open(dest, "w:gz", compresslevel=9) as t:
        dirs = set()
        for arc, _, _ in entries:
            parts = arc.split("/")[:-1]
            for i in range(1, len(parts) + 1):
                dirs.add("/".join(parts[:i]))
        for d in sorted(dirs):
            ti = tarfile.TarInfo(d)
            ti.type, ti.mode, ti.mtime = tarfile.DIRTYPE, 0o755, MTIME
            t.addfile(ti)
        for arc, data, exe in entries:
            ti = tarfile.TarInfo(arc)
            ti.size, ti.mtime, ti.mode = len(data), MTIME, 0o755 if exe else 0o644
            t.addfile(ti, io.BytesIO(data))


def evidence_entries():
    out = []
    for name in EVIDENCE:
        src = RESULTS / name
        if not src.exists():
            print(f"warning: missing evidence {src}", file=sys.stderr)
            continue
        data = src.read_bytes()
        if name == "govulncheck.txt":
            text = data.decode("utf-16") if data[:2] in (b"\xff\xfe", b"\xfe\xff") else data.decode("utf-8-sig")
            data = text.replace("\r\n", "\n").encode("utf-8")
        out.append((f"evidence/{name}", data, False))
    for p in sorted((RESULTS / "payloads").glob("*.json")):
        out.append((f"evidence/payloads/{p.name}", p.read_bytes(), False))
    for name in GENERATORS:
        out.append((f"generators/{name}", (DESIGN / name).read_bytes().replace(b"\r\n", b"\n"), False))
    return out


def main():
    if OUT.exists():
        shutil.rmtree(OUT)
    OUT.mkdir()
    win, unix = bundle_entries(True), bundle_entries(False)
    build_zip(OUT / f"dxps-{VERSION}-windows.zip", win)
    build_tgz(OUT / f"dxps-{VERSION}-ubuntu.tar.gz", unix)
    build_zip(OUT / f"dxps-{VERSION}-evidence.zip", evidence_entries())
    (OUT / "README.txt").write_text(README.format(version=VERSION, date=DATE).replace("\n", "\r\n"),
                                    encoding="utf-8", newline="")

    assets = sorted(p for p in OUT.iterdir() if p.is_file())
    lines = [f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}" for p in assets if p.name != "README.txt"]
    (OUT / "SHA256SUMS.txt").write_text("\n".join(lines) + "\n", encoding="utf-8", newline="\n")

    print(f"{OUT}\n  bundle files: {len(win)}")
    for p in sorted(OUT.iterdir()):
        print(f"  {p.stat().st_size:>10,}  {p.name}")


if __name__ == "__main__":
    main()
