"""Real local binary launcher used by demonstrations and benchmarks."""
from __future__ import annotations
import json
import os
import selectors
import subprocess
import sys
import tempfile
from pathlib import Path
ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "examples"))
from agent_client import Client


def error_details(error):
    details = {'type': type(error).__name__, 'message': str(error)}
    if getattr(error, '__notes__', None):
        details['notes'] = list(error.__notes__)
    return details


class LocalService:
    def __init__(self, binary: Path | None = None):
        self.binary = binary or ROOT / "bin/branchharbor"
        self.temp = tempfile.TemporaryDirectory(prefix="branchharbor-")
        self.root = Path(self.temp.name)
        self.dir = self.root / "store"
        self.process = None
        self.stderr = None
    def command(self, *args: str) -> dict:
        p = subprocess.run([str(self.binary), *args], capture_output=True, text=True, timeout=60)
        if p.returncode:
            raise RuntimeError(f"CLI {args[0]} failed (exit {p.returncode}): {p.stderr.strip()}")
        return json.loads(p.stdout)
    def token(self, subject="operator", branch="*", prefix="", ops="admin") -> Path:
        path = self.root / f"{subject}-{branch.replace('*','all')}.token"
        self.command("token", "--dir", str(self.dir), "--subject", subject, "--branch", branch,
                     "--prefix", prefix, "--ops", ops, "--ttl", "3600", "--out", str(path))
        return path
    def start(self):
        try:
            self.stderr = (self.root / "server.stderr").open("ab")
            self.process = subprocess.Popen([str(self.binary), "serve", "--dir", str(self.dir),
                "--listen", "127.0.0.1:0"], stdout=subprocess.PIPE, stderr=self.stderr, text=True)
            with selectors.DefaultSelector() as sel:
                sel.register(self.process.stdout, selectors.EVENT_READ)
                if not sel.select(timeout=15):
                    raise RuntimeError("Server readiness timed out")
            line = self.process.stdout.readline()
            if not line:
                raise RuntimeError("Server exited before announcing listener")
            self.url = "http://" + json.loads(line)["listening"]
            self.client = Client(self.url, self.admin_token)
            if self.client.request("GET", "/healthz") != {"ready": True}:
                raise RuntimeError("Server is not healthy")
        except BaseException as error:
            try:
                self.stop()
            except Exception as cleanup:
                error.add_note(f"Server cleanup failed: {type(cleanup).__name__}: {cleanup}")
            raise

    def stop(self):
        process, self.process = self.process, None
        stderr, self.stderr = self.stderr, None
        errors = []
        try:
            if process is not None:
                code = process.poll()
                if code is None:
                    process.terminate()
                    try:
                        code = process.wait(timeout=15)
                    except subprocess.TimeoutExpired as timeout:
                        process.kill()
                        process.wait(timeout=15)
                        raise RuntimeError("Server did not shut down gracefully") from timeout
                if code != 0:
                    raise RuntimeError(f"Server shutdown failed: {code}")
        except Exception as error:
            errors.append(error)
        finally:
            for handle in (process.stdout if process is not None else None, stderr):
                if handle is not None:
                    try:
                        handle.close()
                    except Exception as error:
                        errors.append(error)
        if errors:
            for error in errors[1:]:
                errors[0].add_note(f"Additional cleanup failure: {type(error).__name__}: {error}")
            raise errors[0]

    def __enter__(self):
        try:
            self.command("init", "--dir", str(self.dir))
            self.admin_token = self.token()
            self.start()
            return self
        except BaseException as error:
            for cleanup in (self.stop, self.temp.cleanup):
                try:
                    cleanup()
                except Exception as caught:
                    error.add_note(f"Startup cleanup failed: {type(caught).__name__}: {caught}")
            raise

    def __exit__(self, exc_type, error, traceback):
        errors = []
        for cleanup in (self.stop, self.temp.cleanup):
            try:
                cleanup()
            except Exception as caught:
                errors.append(caught)
        if error is not None:
            for caught in errors:
                error.add_note(f"Exit cleanup failed: {type(caught).__name__}: {caught}")
        elif errors:
            for caught in errors[1:]:
                errors[0].add_note(f"Additional cleanup failure: {type(caught).__name__}: {caught}")
            raise errors[0]
        return False
