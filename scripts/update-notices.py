#!/usr/bin/env python3
"""Collect dependency license notices for Leecher's supported builds."""

import json
import os
from pathlib import Path
import shutil
import subprocess


ROOT = Path(__file__).resolve().parents[1]
TARGETS = (
    "linux/amd64", "linux/arm64", "linux/arm",
    "darwin/amd64", "darwin/arm64", "windows/amd64",
)


def objects(text):
    decoder = json.JSONDecoder()
    while text.strip():
        text = text.lstrip()
        value, end = decoder.raw_decode(text)
        yield value
        text = text[end:]


def proxy_escape(value):
    return "".join("!" + c.lower() if c.isupper() else c for c in value)


def main():
    modules = {}
    headers = {}
    for target in TARGETS:
        goos, goarch = target.split("/")
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
        result = subprocess.run(
            ["go", "list", "-deps", "-json", "./..."],
            cwd=ROOT, env=env, check=True, capture_output=True, text=True,
        )
        for package in objects(result.stdout):
            module = package.get("Module")
            if not module or module.get("Main"):
                continue
            name = module["Path"]
            modules[name] = module
            notices = headers.setdefault(name, {})
            for filename in package.get("GoFiles", []) + package.get("CgoFiles", []):
                source = Path(package["Dir"]) / filename
                text = source.read_text()
                header = text.split("\npackage ", 1)[0]
                if "copyright" in header.lower() or "license" in header.lower():
                    notices[str(source.relative_to(module["Dir"]))] = header

    output = ROOT / "third_party"
    records = []
    for name, module in sorted(modules.items()):
        destination = output / "licenses" / (name + "@" + module["Version"])
        destination.mkdir(parents=True, exist_ok=True)
        files = []
        source = Path(module["Dir"])
        for path in sorted(source.rglob("*")):
            if not path.is_file() or not path.name.lower().startswith(
                ("license", "licence", "copying", "notice", "copyright", "authors")
            ):
                continue
            relative = path.relative_to(source)
            target = destination / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, target)
            files.append(str(target.relative_to(ROOT)))
        if not files:
            raise RuntimeError(f"No license notice found for {name}")
        if headers[name]:
            target = destination / "SOURCE_NOTICES.txt"
            target.write_text("\n\n".join(
                f"--- {filename} ---\n{text}"
                for filename, text in sorted(headers[name].items())
            ) + "\n")
            files.append(str(target.relative_to(ROOT)))
        module_path = proxy_escape(name)
        version = proxy_escape(module["Version"])
        records.append({
            "module": name, "version": module["Version"],
            "source": f"https://proxy.golang.org/{module_path}/@v/{version}.zip",
            "notices": files,
        })
    goroot = Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
    go_notices = output / "go"
    go_notices.mkdir(parents=True, exist_ok=True)
    for name in ("LICENSE", "PATENTS"):
        candidates = (goroot / name, goroot.parent / name)
        source = next((p for p in candidates if p.is_file()), None)
        if source is None:
            raise RuntimeError(f"Go distribution is missing {name}")
        shutil.copyfile(source, go_notices / name)
    (output / "modules.json").write_text(json.dumps(records, indent=2) + "\n")
    print(f"Collected notices for {len(records)} compiled Go modules.")


if __name__ == "__main__":
    main()
