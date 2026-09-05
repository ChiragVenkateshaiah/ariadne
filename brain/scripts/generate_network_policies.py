#!/usr/bin/env python3
"""Generates NetworkPolicy YAML from the World Model's CALLS edges and
writes them to deploy/security/generated/. Apply with:
    kubectl apply -f deploy/security/generated/
Usage: python scripts/generate_network_policies.py [db_path] [namespace]
"""
import sys
from pathlib import Path

import yaml

from ariadne.graph import store
from ariadne.security.network_policy import generate_network_policies

ROOT = Path(__file__).resolve().parents[2]

if __name__ == "__main__":
    db_path = sys.argv[1] if len(sys.argv) > 1 else "ariadne.db"
    namespace = sys.argv[2] if len(sys.argv) > 2 else "travel"

    conn = store.connect(db_path)
    policies = generate_network_policies(conn, namespace)

    out_dir = ROOT / "deploy" / "security" / "generated"
    out_dir.mkdir(parents=True, exist_ok=True)
    for f in out_dir.glob("*.yaml"):
        f.unlink()

    for p in policies:
        path = out_dir / f"{p.name}.yaml"
        path.write_text(f"# {p.rationale}\n" + yaml.safe_dump(p.manifest, sort_keys=False))
        print(f"wrote {path.relative_to(ROOT)}")
        print(f"  {p.rationale}")
