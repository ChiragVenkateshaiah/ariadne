#!/usr/bin/env python3
"""Runs the topology ingester against a live sensor, persisting to a SQLite
file. Usage: python scripts/run_ingester.py [sensor_addr] [db_path]"""
import sys

from ariadne.graph import store
from ariadne.graph.ingest import run

if __name__ == "__main__":
    sensor_addr = sys.argv[1] if len(sys.argv) > 1 else "localhost:9090"
    db_path = sys.argv[2] if len(sys.argv) > 2 else "ariadne.db"
    conn = store.connect(db_path)
    print(f"connecting to sensor at {sensor_addr}, writing to {db_path}", file=sys.stderr)
    run(conn, sensor_addr)
