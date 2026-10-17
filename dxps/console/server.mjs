// HTTPS front for the production build: the console is served over TLS only (DXPS_CONSOLE_CERT / DXPS_CONSOLE_KEY).
import { readFileSync } from "node:fs";
import { createServer } from "node:https";
import next from "next";

const port = Number(process.env.DXPS_CONSOLE_PORT || 8092);
const host = process.env.DXPS_CONSOLE_HOST || "127.0.0.1";
const cert = process.env.DXPS_CONSOLE_CERT;
const key = process.env.DXPS_CONSOLE_KEY;
if (!cert || !key) {
  console.error("DXPS_CONSOLE_CERT and DXPS_CONSOLE_KEY are required");
  process.exit(1);
}

const app = next({ dev: false, hostname: host, port });
const handle = app.getRequestHandler();
await app.prepare();
createServer({ cert: readFileSync(cert), key: readFileSync(key), minVersion: "TLSv1.2" }, (req, res) => handle(req, res)).listen(
  port,
  host,
  () => console.log(JSON.stringify({ level: "INFO", component: "dxps-console-ui", msg: "listening", url: `https://${host}:${port}` })),
);
