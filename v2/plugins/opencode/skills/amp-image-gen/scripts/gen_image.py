#!/usr/bin/env python3
"""Generate or edit an image through OpenRouter and save it as a file.

  gen_image.py "prompt" -o out.png [--input ref.png ...] [--model ID] [--aspect 16:9]

The key comes from OPENROUTER_API_KEY, or from opencode's stored OpenRouter key
(auth.json in opencode's data directory). Prints the saved path(s) and the cost.
"""
import argparse, base64, json, mimetypes, os, sys, urllib.request
from pathlib import Path

DEFAULT_MODEL = "google/gemini-3.1-flash-image"  # cheap drafts; google/gemini-nano-banana-2.1 for finals


def api_key():
    if os.environ.get("OPENROUTER_API_KEY"):
        return os.environ["OPENROUTER_API_KEY"]
    data = Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share") / "opencode/auth.json"
    try:
        return json.loads(data.read_text())["openrouter"]["key"]
    except Exception:
        sys.exit("No OpenRouter key: set OPENROUTER_API_KEY or run `opencode auth login` for OpenRouter.")


def data_url(path):
    mime = mimetypes.guess_type(path)[0] or "image/png"
    return f"data:{mime};base64,{base64.b64encode(Path(path).read_bytes()).decode()}"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("prompt")
    ap.add_argument("-o", "--out", required=True, help="output file; numbered if the model returns several images")
    ap.add_argument("--input", action="append", default=[], help="reference or source image to edit (repeatable)")
    ap.add_argument("--model", default=DEFAULT_MODEL)
    ap.add_argument("--aspect", help="aspect ratio, e.g. 1:1, 16:9, 9:16")
    a = ap.parse_args()

    content = [{"type": "text", "text": a.prompt}] + [{"type": "image_url", "image_url": {"url": data_url(p)}} for p in a.input]
    body = {"model": a.model, "modalities": ["image", "text"], "messages": [{"role": "user", "content": content}], "usage": {"include": True}}
    if a.aspect:
        body["image_config"] = {"aspect_ratio": a.aspect}
    req = urllib.request.Request("https://openrouter.ai/api/v1/chat/completions", data=json.dumps(body).encode(),
                                 headers={"Authorization": f"Bearer {api_key()}", "Content-Type": "application/json",
                                          "X-Title": "AMP image generation"})
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            res = json.load(r)
    except urllib.error.HTTPError as e:
        sys.exit(f"OpenRouter error {e.code}: {e.read().decode()[:500]}")

    msg = (res.get("choices") or [{}])[0].get("message", {})
    images = msg.get("images") or []
    if not images:
        sys.exit(f"No image returned. Model said: {(msg.get('content') or '')[:500]}")
    out = Path(a.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    for i, img in enumerate(images):
        url = img["image_url"]["url"]
        raw = base64.b64decode(url.split(",", 1)[1])
        path = out if len(images) == 1 else out.with_name(f"{out.stem}-{i + 1}{out.suffix}")
        path.write_bytes(raw)
        print(path)
    if msg.get("content"):
        print(f"note: {msg['content'].strip()[:300]}")
    cost = (res.get("usage") or {}).get("cost")
    print(f"model: {a.model}" + (f"  cost: ${cost:.4f}" if isinstance(cost, (int, float)) else ""))


if __name__ == "__main__":
    main()
