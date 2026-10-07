# DxPS - Real-time Provisioning System

Multi-tenant (MNO / MVNO / enterprise) real-time provisioning platform built on Go, PostgreSQL 18 and Apache Kafka 4 (KRaft),
with network-element simulators and the Mosaic live dashboard.

| Path | Contents |
|---|---|
| `dxps/` | Go module: services (`cmd/`), packages and tests (`internal/`), install/run scripts (`scripts/`) |
| `dxps/INSTALL.md` | Step-by-step installation and running guide for Windows, Ubuntu and macOS |
| `DxPS_TO-BE_HLD_LLD.docx/.pdf` | TO-BE design (HLD + LLD) |
| `DxPS_Test_Cases_and_Results.docx/.pdf` | Test cases, results, coverage, security scans and payloads |
| `DxPS_Installation_and_Running_Guide.docx/.pdf` | Installation guide (Word / PDF) |
| `design/` | Generators for the documents and diagrams, and the delivery packager |

## Quick start

```bash
# Ubuntu 22.04/24.04 or macOS 13+ (normal user with sudo)
cd dxps
bash scripts/install-deps-ubuntu.sh   # or: bash scripts/install-deps-macos.sh
bash scripts/infra-setup.sh
bash scripts/run-all.sh
```

```powershell
# Windows 10/11
cd dxps
powershell -ExecutionPolicy Bypass -File scripts\install-deps-windows.ps1
powershell -ExecutionPolicy Bypass -File scripts\infra-setup.ps1
powershell -ExecutionPolicy Bypass -File scripts\run-all.ps1
```

Then open the Mosaic dashboard at http://127.0.0.1:8088. Run the tests with `scripts/test-all` and the security scans with `scripts/sec-scan`.

Secrets, keys and the development PKI are generated locally by `infra-setup` into the runtime directory and are never committed.
