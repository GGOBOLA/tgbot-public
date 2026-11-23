"""Integration regressions for scripts/reconcile.sh, using a temporary SQLite book."""
import http.server
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.parse


class _API(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        form = urllib.parse.parse_qs(body.decode())
        self.server.received.append(form["text"][0])
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')

    def log_message(self, *_args):
        pass


class ReconcileTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        (root / "scripts" / "lib").mkdir(parents=True)
        source = Path(__file__).resolve().parent
        shutil.copy2(source / "reconcile.sh", root / "scripts" / "reconcile.sh")
        shutil.copy2(source / "lib" / "telegram.sh", root / "scripts" / "lib" / "telegram.sh")

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _API)
        self.server.received = []
        thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

        self.db_path = root / "book.db"
        env = "DB_PATH=" + str(self.db_path) + "\n"
        env += "TELEGRAM_BOT_TOKEN=test-token\nADMIN_IDS=7\n"
        env += "TELEGRAM_API_URL=http://127.0.0.1:" + str(self.server.server_port) + "\n"
        (root / ".env").write_text(env)
        self.root = root
        self.script = root / "scripts" / "reconcile.sh"

    def db(self):
        c = sqlite3.connect(self.db_path)
        c.executescript("""
        create table customers (telegram_id integer primary key, balance integer not null);
        create table usage (id integer primary key, telegram_id integer, model text,
          cost integer, created_at integer, estimated integer);
        create table ledger (id integer primary key, telegram_id integer, delta integer,
          balance_after integer, reason text, ref text, created_at integer);
        """)
        return c

    @staticmethod
    def topup(c, timestamp):
        # Keep the balance-vs-ledger invariant true, so each test isolates drift.
        c.execute("insert into customers values (1, 9000)")
        c.execute("insert into ledger values (900, 1, 10000, 10000, 'topup', 'seed', ?)", (timestamp,))
        c.commit()

    def validate_check(self, expected_drift):
        env = os.environ.copy()
        env.pop("DB_PATH", None)
        result = subprocess.run(["bash", str(self.script)], cwd=self.root,
                                env=env, text=True, capture_output=True, timeout=15)
        output = result.stdout + result.stderr
        match = re.search(r"drift=(\d+)usen", output)
        self.assertIsNotNone(match, output)
        self.assertEqual(int(match.group(1)), expected_drift, output)
        if expected_drift:
            self.assertNotEqual(result.returncode, 0, output)
            self.assertGreater(len(self.server.received), 0, output)
            self.assertIn("Reconciliation failed", self.server.received[-1])
        else:
            self.assertEqual(result.returncode, 0, output)
        self.assertIn("bad_customers=0", output)

    def test_current_usage_reference_joins_to_own_ledger_row(self):
        c = self.db()
        timestamp = int(time.time())
        c.executemany("insert into usage values (?, 1, 'm', 1000, ?, ?)", [
            (1, timestamp, 0), (2, timestamp, 1),
        ])
        c.execute("insert into ledger values (1, 1, -1000, 9000, 'usage', 'usage:1', ?)", (timestamp,))
        c.execute("insert into ledger values (2, 1, -1000, 8000, 'usage', 'usage:2', ?)", (timestamp,))
        self.topup(c, timestamp)
        c.execute("update customers set balance = 8000")
        c.commit()
        self.validate_check(0)

    def test_legacy_unbilled_call_is_reported(self):
        c = self.db()
        timestamp = int(time.time())
        c.executemany("insert into usage values (?, 1, 'm', 1000, ?, ?)", [
            (1, timestamp, 0), (2, timestamp, 1),
        ])
        c.execute("insert into ledger values (1, 1, -1000, 9000, 'usage', '', ?)", (timestamp,))
        self.topup(c, timestamp)
        self.validate_check(1000)

    def test_legacy_one_estimate_cannot_hide_duplicate_ledger_row(self):
        c = self.db()
        timestamp = int(time.time())
        c.execute("insert into usage values (1, 1, 'm', 1000, ?, 1)", (timestamp,))
        c.executemany("insert into ledger values (?, 1, -1000, 8000, 'usage', '', ?)", [
            (1, timestamp), (2, timestamp),
        ])
        c.execute("insert into customers values (1, 8000)")
        c.execute("insert into ledger values (900, 1, 10000, 10000, 'topup', 'seed', ?)", (timestamp,))
        c.commit()
        self.validate_check(1000)


if __name__ == "__main__":
    unittest.main()
