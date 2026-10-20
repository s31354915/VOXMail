#!/usr/bin/env python3
"""VOXMail resident Piper worker.

Loads one Piper voice model once and serves synthesis requests over
stdin/stdout, so a call pays the model-load cost once instead of once per
dynamic prompt. The Go parent starts one worker per voice model and shares
it across every call that uses that model.

Protocol (one JSON object per line):
  in:  {"id": N, "text": "...", "length_scale": 1.25, "out": "/path/tmp.wav"}
  out: {"id": N, "ok": true}
       {"id": N, "ok": false, "error": "..."}
After the model loads the worker writes {"ready": true}; a load failure
writes {"ready": false, "error": "..."} and exits nonzero. EOF on stdin
ends the worker cleanly.
"""

import argparse
import json
import signal
import sys
import wave

from piper import PiperVoice, SynthesisConfig


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def main():
    parser = argparse.ArgumentParser(description="VOXMail resident Piper worker")
    parser.add_argument("--model", required=True)
    parser.add_argument("--config")
    args = parser.parse_args()

    def stop(*_):
        sys.exit(0)

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    try:
        voice = PiperVoice.load(args.model, config_path=args.config)
    except BaseException as exc:  # a broken model must answer, not hang the parent
        emit({"ready": False, "error": "%s: %s" % (type(exc).__name__, exc)})
        return 1
    emit({"ready": True})

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            request = json.loads(line)
        except ValueError as exc:
            emit({"id": -1, "ok": False, "error": "bad request: %s" % exc})
            continue
        request_id = request.get("id")
        try:
            text = request.get("text") or ""
            out = request.get("out") or ""
            if not text or not out:
                raise ValueError("text and out are required")
            config = SynthesisConfig()
            length_scale = request.get("length_scale")
            if length_scale is not None:
                config.length_scale = float(length_scale)
            with wave.open(out, "wb") as wav_file:
                voice.synthesize_wav(text, wav_file, syn_config=config)
            emit({"id": request_id, "ok": True})
        except BaseException as exc:  # one bad request must not kill the worker
            emit({"id": request_id, "ok": False, "error": "%s: %s" % (type(exc).__name__, exc)})
    return 0


if __name__ == "__main__":
    sys.exit(main())
