"""Reads an image config (crane config) on stdin and prints a final Dockerfile stage that flattens the
`patched` stage into one layer while keeping the original image's runtime config."""
import json
import sys

c = json.load(sys.stdin)["config"]
out = ["", "FROM scratch", "COPY --from=patched / /"]
for e in c.get("Env") or []:
    k, _, v = e.partition("=")
    out.append(f"ENV {k}={json.dumps(v)}")
for k, v in (c.get("Labels") or {}).items():
    out.append(f"LABEL {json.dumps(k)}={json.dumps(v)}")
for port in c.get("ExposedPorts") or {}:
    out.append(f"EXPOSE {port}")
if c.get("Volumes"):
    out.append(f"VOLUME {json.dumps(sorted(c['Volumes']))}")
if c.get("WorkingDir"):
    out.append(f"WORKDIR {c['WorkingDir']}")
if c.get("StopSignal"):
    out.append(f"STOPSIGNAL {c['StopSignal']}")
if c.get("User"):
    out.append(f"USER {c['User']}")
if c.get("Entrypoint"):
    out.append(f"ENTRYPOINT {json.dumps(c['Entrypoint'])}")
if c.get("Cmd"):
    out.append(f"CMD {json.dumps(c['Cmd'])}")
print("\n".join(out))
