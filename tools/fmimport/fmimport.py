#!/usr/bin/env python3
"""Import the foxess_modbus register maps (design D-021).

Reads a checkout of https://github.com/nathanmarlor/foxess_modbus (MIT,
(c) 2021 nathanmarlor) WITHOUT executing it:

- the profile table (`_INVERTER_PROFILES_LIST` in inverter_profiles.py),
  parsed with `ast`: model, model-string pattern, connection types,
  register type, and the version -> generation (`Inv`) map;
- the serialized entity maps the project's own test suite snapshots
  (tests/__snapshots__/test_entity_descriptions/*.json), one per model /
  connection / version. A map depends only on (generation, register
  type), so maps are deduplicated under that key.

Writes one JSON file (internal/fmdata/foxess_modbus.json) carrying the
source commit and licence. Stdlib only. Usage:

    git clone https://github.com/nathanmarlor/foxess_modbus /tmp/fm
    git -C /tmp/fm checkout <commit>
    python3 -I tools/fmimport/fmimport.py --src /tmp/fm \
        --out internal/fmdata/foxess_modbus.json
"""

import argparse
import ast
import json
import pathlib
import subprocess
import sys

PKG = "custom_components/foxess_modbus"
SNAPS = "tests/__snapshots__/test_entity_descriptions"


def enum_values(types_py: pathlib.Path, cls: str) -> dict:
    """NAME -> value for a str enum class (InverterModel)."""
    tree = ast.parse(types_py.read_text())
    for node in tree.body:
        if isinstance(node, ast.ClassDef) and node.name == cls:
            out = {}
            for st in node.body:
                if (isinstance(st, ast.Assign) and len(st.targets) == 1
                        and isinstance(st.targets[0], ast.Name)
                        and isinstance(st.value, ast.Constant)):
                    out[st.targets[0].id] = st.value.value
            return out
    sys.exit(f"{cls} not found in {types_py}")


def attr(node: ast.AST) -> str:
    """Enum.MEMBER -> MEMBER."""
    if isinstance(node, ast.Attribute):
        return node.attr
    sys.exit(f"expected an enum member, got {ast.dump(node)}")


def version(node: ast.AST):
    """Version(a, b) -> "a.b"; None -> None."""
    if isinstance(node, ast.Constant) and node.value is None:
        return None
    if isinstance(node, ast.Call) and getattr(node.func, "id", "") == "Version":
        return ".".join(str(a.value) for a in node.args)
    sys.exit(f"unexpected version key {ast.dump(node)}")


def kwarg(call: ast.Call, name: str, pos: int):
    for k in call.keywords:
        if k.arg == name:
            return k.value
    return call.args[pos] if len(call.args) > pos else None


def special_registers(tree: ast.Module) -> dict:
    """NAME -> {invalid_ranges, individual_ranges} for every module-level
    `NAME = SpecialRegisterConfig(...)` (literal tuples only)."""
    out = {}
    for node in tree.body:
        if (isinstance(node, ast.Assign) and isinstance(node.targets[0], ast.Name)
                and isinstance(node.value, ast.Call)
                and getattr(node.value.func, "id", "") == "SpecialRegisterConfig"):
            kw = {k.arg: ast.literal_eval(k.value) for k in node.value.keywords}
            out[node.targets[0].id] = {
                "invalid_ranges": [list(r) for r in kw.get("invalid_register_ranges", [])],
                "individual_ranges": [list(r) for r in kw.get("individual_read_register_ranges", [])],
            }
    return out


def profiles(src: pathlib.Path, models: dict) -> list:
    tree = ast.parse((src / PKG / "inverter_profiles.py").read_text())
    specials = special_registers(tree)
    lst = None
    for node in tree.body:
        if (isinstance(node, ast.Assign) and isinstance(node.targets[0], ast.Name)
                and node.targets[0].id == "_INVERTER_PROFILES_LIST"):
            lst = node.value
    if not isinstance(lst, ast.List):
        sys.exit("_INVERTER_PROFILES_LIST not found")
    out = []
    for elt in lst.elts:
        conns = []
        call = elt
        # unwind InverterModelProfile(...).add_connection_type(...)...
        while isinstance(call.func, ast.Attribute) and call.func.attr == "add_connection_type":
            versions = kwarg(call, "versions", 2)
            sp = kwarg(call, "special_registers", 3)
            special = {"invalid_ranges": [], "individual_ranges": []}
            if sp is not None:
                if not isinstance(sp, ast.Name) or sp.id not in specials:
                    sys.exit(f"unresolved special_registers {ast.dump(sp)}")
                special = specials[sp.id]
            conns.append({
                "connection": attr(kwarg(call, "connection_type", 0)),
                "register_type": attr(kwarg(call, "register_type", 1)),
                **special,
                "versions": [{"max_manager_version": version(k), "inv": attr(v)}
                             for k, v in zip(versions.keys, versions.values)],
            })
            call = call.func.value
        if getattr(call.func, "id", "") != "InverterModelProfile":
            sys.exit(f"unexpected profile shape: {ast.dump(call)[:200]}")
        model = models[attr(call.args[0])]
        pattern = call.args[1].value
        conns.reverse()
        out.append({"model": model, "pattern": pattern, "connections": conns})
    return out


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", required=True, type=pathlib.Path)
    ap.add_argument("--out", required=True, type=pathlib.Path)
    a = ap.parse_args()

    git = lambda *args: subprocess.run(["git", "-C", str(a.src), *args],
                                       check=True, capture_output=True, text=True).stdout.strip()
    commit, date = git("log", "-1", "--format=%H %cs").split()
    licence = (a.src / "LICENSE").read_text()
    if "MIT License" not in licence:
        sys.exit("upstream licence is no longer MIT: stop and review")

    models = enum_values(a.src / PKG / "common/types.py", "InverterModel")
    profs = profiles(a.src, models)

    maps = {}
    for p in profs:
        for c in p["connections"]:
            for v in c["versions"]:
                tag = "latest" if v["max_manager_version"] is None else "v" + v["max_manager_version"]
                snap = a.src / SNAPS / f"test_entities[{p['model']}-{c['connection']}-{tag}].json"
                if not snap.exists():
                    sys.exit(f"missing snapshot {snap.name}")
                key = f"{v['inv']}-{c['register_type']}"
                entities = json.loads(snap.read_text())
                if key in maps and maps[key] != entities:
                    sys.exit(f"map {key} differs between snapshots: the dedupe premise is wrong")
                maps[key] = entities
                v["map"] = key

    out = {
        "source": {
            "repo": "https://github.com/nathanmarlor/foxess_modbus",
            "commit": commit,
            "date": date,
            "license": "MIT",
            "notice": licence.strip().splitlines()[2] if len(licence.splitlines()) > 2 else "",
        },
        "note": ("Generated by tools/fmimport/fmimport.py from the upstream test snapshots; "
                 "do not edit. 32-bit addresses are listed low word first. "
                 "max_manager_version is upstream's generation threshold, which does not "
                 "hold on the reference KH10 (manager 1.71 answers the KH_PRE133 map)."),
        "profiles": profs,
        "maps": dict(sorted(maps.items())),
    }
    a.out.write_text(json.dumps(out, indent=1, sort_keys=False) + "\n")
    print(f"{len(profs)} profiles, {len(maps)} maps from {commit[:12]} ({date}) -> {a.out}")


if __name__ == "__main__":
    main()
