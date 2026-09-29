"""Read-only example; use an isolated synthetic project and an existing session."""
import json
import os
from lantai import APIError, Client

client = Client(os.environ['LANTAI_SERVER'], os.environ['LANTAI_SESSION_TOKEN'])
try:
    client.meta()
    page = client.tasks(os.environ['LANTAI_PROJECT_ID'], limit=20)
    print(json.dumps(page, ensure_ascii=False))
except APIError as error:
    print(json.dumps({'code': error.code, 'retryable': error.retryable,
                      'recovery_action': error.recovery_action,
                      'operation_id': error.operation_id}))
    raise SystemExit(1)
