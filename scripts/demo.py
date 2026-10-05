#!/usr/bin/env python3
"""Exercise a real server with two scoped, deterministic agent clients."""
import base64
import concurrent.futures
import json
from local_service import LocalService, Client
from agent_client import APIError

def expect_error(fn, status, code):
    try:
        fn()
    except APIError as exc:
        assert (exc.status, exc.code) == (status, code), str(exc)
    else:
        raise AssertionError("Expected authorization/conflict rejection")

def main():
    events=[]
    with LocalService() as service:
        admin=service.client
        original=admin.head("main")
        for branch in ("agent-a","agent-b"):
            admin.request("POST","/v1/fork",{"branch":branch,"source":"main","expected":original,"request_id":"fork-"+branch})
        a=Client(service.url,service.token("alice","agent-a","docs/","read,write"))
        b=Client(service.url,service.token("bob","agent-b","docs/","read,write"))
        ah,bh=a.head("agent-a"),b.head("agent-b")
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            x=pool.submit(a.commit,"agent-a",ah,"alice-1",{"docs/a.txt":b"Alice proposal"})
            y=pool.submit(b.commit,"agent-b",bh,"bob-1",{"docs/b.txt":b"Bob proposal"})
            ar,br=x.result(),y.result()
        events.append({"step":"isolated_parallel_proposals","main_unchanged":admin.head("main")==original})
        assert admin.head("main")==original
        expect_error(lambda:a.request("GET","/v1/head?branch=agent-b"),403,"forbidden")
        expect_error(lambda:a.commit("agent-a",ar["head"],"escape",{"private/file":b"denied"}),403,"forbidden")
        expect_error(lambda:a.request("POST","/v1/merge",{}),403,"forbidden")
        events.append({"step":"branch_prefix_and_promotion_denials","status":"PASS"})
        for branch,result in (("agent-a",ar),("agent-b",br)):
            preview=admin.request("POST","/v1/merge/preview",{"target":"main","source":branch})
            assert not preview["conflicts"]
            admin.request("POST","/v1/merge",{"target":"main","source":branch,"expected_target":admin.head("main"),"expected_source":result["head"],"request_id":"promote-"+branch})
        files=admin.request("GET","/v1/files?branch=main")["files"]
        assert [f["path"] for f in files]==["docs/a.txt","docs/b.txt"]
        events.append({"step":"reviewed_disjoint_merges","files":[f["path"] for f in files]})
        # Same-branch stale update and restart-resolved idempotency.
        current=admin.head("main")
        saved=admin.commit("main",current,"durable-once",{"docs/stable.txt":b"persisted"})
        expect_error(lambda:admin.commit("main",current,"stale",{"docs/stale.txt":b"no"}),409,"conflict")
        service.stop();service.start();admin=service.client
        again=admin.commit("main",current,"durable-once",{"docs/stable.txt":b"persisted"})
        assert again["replayed"] and again["head"]==saved["head"]
        events.append({"step":"restart_and_retry_identity","status":"PASS"})
        # Explicit whole-file conflict, without an automatic overwrite.
        head=admin.head("main")
        admin.request("POST","/v1/fork",{"branch":"conflicting","source":"main","expected":head,"request_id":"fork-conflict"})
        admin.commit("main",head,"ours",{"docs/stable.txt":b"ours"})
        admin.commit("conflicting",admin.head("conflicting"),"theirs",{"docs/stable.txt":b"theirs"})
        preview=admin.request("POST","/v1/merge/preview",{"target":"main","source":"conflicting"})
        assert preview["conflicts"]==["docs/stable.txt"]
        expect_error(lambda:admin.request("POST","/v1/merge",{"target":"main","source":"conflicting","expected_target":preview["target"],"expected_source":preview["source"],"request_id":"conflict"}),409,"conflict")
        events.append({"step":"conflict_requires_resolution","conflicts":preview["conflicts"]})
        # Contents are not printed by the service, even though they were stored.
        service.stop()
        logs=(service.root/"server.stderr").read_bytes()
        for secret in (b"Alice proposal",b"Bob proposal",service.admin_token.read_bytes().strip()):
            assert secret not in logs
        events.append({"step":"service_log_content_check","status":"PASS"})
    print(json.dumps({"status":"PASS","type":"real_local_http_demo","live_model_used":False,"events":events},indent=2))
if __name__=="__main__":main()
