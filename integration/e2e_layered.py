"""Real-provider E2E. Run against an isolated, configured local server.

Usage: python3 integration/e2e_layered.py http://127.0.0.1:18092 VIDEO.mp4
"""
import json
import sys
import time
import urllib.request
import urllib.error
from pathlib import Path

base, video = sys.argv[1:3]

def request(path, body=None, content_type="application/json"):
    if isinstance(body, dict):
        body = json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=body)
    if body is not None:
        req.add_header("Content-Type", content_type)
    with urllib.request.urlopen(req, timeout=600) as response:
        data = json.load(response)
    assert data["ok"], data
    return data["result"]

def expect_error(path, body, status):
    raw = json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=raw, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=60)
    except urllib.error.HTTPError as exc:
        assert exc.code == status, (exc.code, exc.read().decode())
        return
    raise AssertionError("expected HTTP %d" % status)

started = time.monotonic()
project = ({"id":sys.argv[3]} if len(sys.argv)>3 else
           request("/v1/ui/projects", {"name": "分层剪辑真实端到端"}))
boundary = "video-agent-e2e-upload"
payload = (f'--{boundary}\r\nContent-Disposition: form-data; name="video"; filename="source.mp4"\r\nContent-Type: video/mp4\r\n\r\n'.encode()
           + Path(video).read_bytes() + f"\r\n--{boundary}--\r\n".encode())
asset = request("/v1/ui/projects/" + project["id"] + "/assets", payload,
                "multipart/form-data; boundary=" + boundary)["asset"]
scope = {"project_id": project["id"], "asset_id": asset["id"]}
task = request("/v1/ui/analyze", scope)
print(json.dumps({"project": project["id"], "analysis": task["id"]}), flush=True)
deadline = time.monotonic() + 900
while time.monotonic() < deadline:
    task = request("/v1/ui/analysis/" + task["id"]) | {"id": task["id"]}
    if task["status"] == "completed":
        break
    assert task["status"] not in ("failed", "cancelled"), task.get("error")
    time.sleep(3)
else:
    raise TimeoutError("analysis exceeded 15 minutes")
print(json.dumps({"analysis_seconds": round(time.monotonic()-started, 1),
                  "evidence": len(task["evidence"])}), flush=True)
proposal = request("/v1/ui/proposals", scope | {"message": "保留数学问题的背景、关键结论与转折，剪成约60秒高光集锦，优先保留完整语句"})
draft = proposal["draft"]
route = "/v1/ui/proposals/" + draft["id"]
c = draft["candidates"][0]
# Exercise a frame-aligned bounds operation, then lock and stale-version rejection.
draft = request(route + "/operations", {"base_version": draft["version"], "edit": {
    "candidate_id": c["id"], "kind": "adjust_bounds", "start_us": c["start_us"], "end_us": c["end_us"]}})
draft = request(route + "/operations", {"base_version": draft["version"], "edit": {
    "candidate_id": c["id"], "kind": "lock"}})
assert draft["candidates"][0]["locked"]
# Optimistic concurrency and lock guards must reject unsafe edits.
expect_error(route + "/operations", {"base_version": draft["version"] - 1, "edit": {
    "candidate_id": c["id"], "kind": "delete"}}, 409)
expect_error(route + "/operations", {"base_version": draft["version"], "edit": {
    "candidate_id": c["id"], "kind": "adjust_bounds", "start_us": c["start_us"], "end_us": c["end_us"]}}, 400)
timeline = request(route + "/confirm", {"version": draft["version"]})
print(json.dumps({"timeline": timeline["id"], "clips": len(timeline["items"]),
                  "seconds": sum(c["source_out_us"]-c["source_in_us"] for c in timeline["items"])/1e6}), flush=True)
for preview in (True, False):
    job = request("/v1/tools/render_submit", {"timeline_id": timeline["id"], "preview": preview})
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        job = request("/v1/jobs/" + job["id"])
        if job["status"] == "completed":
            break
        assert job["status"] not in ("failed", "cancelled"), job
        time.sleep(2)
    else:
        raise TimeoutError("render exceeded five minutes")
    with urllib.request.urlopen(base + "/v1/artifacts/" + job["id"]) as artifact:
        assert artifact.read(32), "empty artifact"
    print(json.dumps({"kind": job["kind"], "output": job["output"], "validation": job.get("validation")},
                     ensure_ascii=False), flush=True)
print("PASS", flush=True)
