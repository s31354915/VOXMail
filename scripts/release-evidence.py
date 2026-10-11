#!/usr/bin/env python3
"""Record exact runtime acceptance and assemble a bounded release evidence record."""
import hashlib
import json
import os
import pathlib
import re
import sys


def read_digest(path):
    value = path.read_text().strip()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", value):
        raise ValueError("invalid image digest")
    return value


def runtime(directory):
    digest = read_digest(pathlib.Path("digests/digest"))
    source = os.environ["SOURCE_SHA"]
    platform = os.environ["ACCEPT_PLATFORM"]
    if not re.fullmatch(r"[0-9a-f]{40}", source) or platform not in ("linux/amd64", "linux/arm64"):
        raise ValueError("invalid acceptance identity")
    directory.mkdir(parents=True, exist_ok=True)
    data = dict(source_sha=source, image=os.environ["IMAGE"] + "@" + digest,
                platform=platform, sip=os.environ.get("SIP_OUTCOME") or "not_run",
                real_models=os.environ.get("MODEL_OUTCOME") or "not_run",
                real_models_required=os.environ["TAG"] != "latest")
    (directory / "acceptance.json").write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")


def assemble(directory):
    source = os.environ.get("SOURCE_SHA", "")
    if source and not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("invalid source revision")
    results = {name: os.environ.get(name.upper().replace("-", "_") + "_RESULT", "unknown")
               for name in ("verify", "require-ci", "build-native", "scan-native", "runtime-native", "publish-manifest")}
    run_id, attempt = os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"]
    image, tag = os.environ["IMAGE"], os.environ["TAG"]
    record = dict(source_sha=source or None, image=image, tag=tag, run_id=run_id,
                  run_attempt=attempt, job_results=results, architectures=[], evidence_errors=[],
                  manifest_digest=os.environ.get("MANIFEST_DIGEST") or None,
                  latest_promotion=os.environ.get("LATEST_OUTCOME") or "not_run")
    for arch in ("amd64", "arm64"):
        scan_name = f"voxmail-scan-{arch}-{run_id}-{attempt}"
        acceptance_name = f"voxmail-acceptance-{arch}-{run_id}-{attempt}"
        item = dict(platform="linux/" + arch, digest=None, scan=None, acceptance=None,
                    scan_artifact=scan_name, acceptance_artifact=acceptance_name)
        try:
            item["digest"] = read_digest(directory / f"voxmail-digest-{arch}" / "digest")
            expected = image + "@" + item["digest"]
            scan = json.loads((directory / scan_name / "identity.json").read_text())
            if scan["source_sha"] != source or scan["image"] != expected or scan["platform"] != item["platform"]:
                raise ValueError("scan source/digest/platform mismatch")
            raw = (directory / scan_name / "trivy.json").read_bytes()
            if scan.get("report_status") != "available" or hashlib.sha256(raw).hexdigest() != scan.get("report_sha256"):
                raise ValueError("scan report missing or hash mismatch")
            if scan.get("severity") != ["HIGH", "CRITICAL"] or scan.get("ignore_unfixed") is not True:
                raise ValueError("scan policy mismatch")
            item["scan"] = scan
            acceptance = json.loads((directory / acceptance_name / "acceptance.json").read_text())
            if acceptance["source_sha"] != source or acceptance["image"] != expected or acceptance["platform"] != item["platform"]:
                raise ValueError("acceptance source/digest/platform mismatch")
            item["acceptance"] = acceptance
            if scan["scan_outcome"] != "success" or acceptance["sip"] != "success":
                raise ValueError("scan or SIP acceptance did not succeed")
            if acceptance["real_models_required"] != (tag != "latest"):
                raise ValueError("real-model requirement mismatch")
            if tag != "latest" and acceptance["real_models"] != "success":
                raise ValueError("required real-model acceptance did not succeed")
        except (OSError, ValueError, KeyError, TypeError) as error:
            record["evidence_errors"].append(arch + ": " + str(error))
        record["architectures"].append(item)
    digest = record["manifest_digest"]
    if digest and not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        record["evidence_errors"].append("invalid published manifest digest")
    for name in ("verify", "require-ci", "build-native", "scan-native", "runtime-native"):
        if results[name] != "success":
            record["evidence_errors"].append(f"required job {name}={results[name]}")
    record["acceptance_complete"] = not record["evidence_errors"]
    return record


def main():
    if len(sys.argv) != 3 or sys.argv[1] not in ("runtime", "summary", "validate"):
        raise SystemExit("usage: release-evidence.py runtime|summary|validate DIRECTORY")
    directory = pathlib.Path(sys.argv[2])
    if sys.argv[1] == "runtime":
        runtime(directory)
        return
    record = assemble(directory)
    if sys.argv[1] == "validate":
        if not record["acceptance_complete"]:
            raise SystemExit("release evidence incomplete: " + "; ".join(record["evidence_errors"]))
        return
    output = pathlib.Path("release-summary")
    output.mkdir(exist_ok=True)
    (output / "release.json").write_text(json.dumps(record, indent=2, sort_keys=True) + "\n")
    lines = ["## VOXMail release evidence", "", f"Source: `{record['source_sha'] or 'unavailable'}`",
             f"Manifest digest: `{record['manifest_digest'] or 'not recorded'}`", "",
             "| Platform | Tested digest | Scan | SIP | Real models |", "| --- | --- | --- | --- | --- |"]
    for item in record["architectures"]:
        scan, acceptance = item["scan"] or {}, item["acceptance"] or {}
        lines.append(f"| {item['platform']} | `{item['digest'] or 'unavailable'}` | {scan.get('scan_outcome', 'unavailable')} | {acceptance.get('sip', 'unavailable')} | {acceptance.get('real_models', 'unavailable')} |")
    lines += ["", "Job results: " + ", ".join(f"{k}={v}" for k, v in record["job_results"].items()),
              "", "Latest promotion: " + record["latest_promotion"],
              "", "Complete acceptance evidence: " + str(record["acceptance_complete"]).lower(),
              "", "Download release.json and the named scan/acceptance artifacts from this run for exact identities and report hashes."]
    if record["evidence_errors"]:
        lines += ["", "Evidence is incomplete; see release.json for missing or mismatched facts."]
    markdown = "\n".join(lines) + "\n"
    (output / "release.md").write_text(markdown)
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as destination:
            destination.write(markdown)


if __name__ == "__main__":
    main()
