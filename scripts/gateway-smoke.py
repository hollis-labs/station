#!/usr/bin/env python3
"""Bounded live dogfood checks. Reads credentials from files; never prints them.
Uses configured dogfood aliases. --live makes billable inference calls.
"""
import argparse
import json
from pathlib import Path
import time
import urllib.request
import urllib.error
import uuid

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--base-url', required=True, help='API root ending in /v1')
    p.add_argument('--token-file', type=Path, required=True)
    p.add_argument('--operator-token-file', type=Path)
    p.add_argument('--output', type=Path)
    p.add_argument('--live', action='store_true')
    a = p.parse_args()
    if not (a.base_url.startswith('https://') or a.base_url.startswith('http://127.0.0.1:')):
        p.error('use HTTPS or explicit loopback')
    token = a.token_file.read_text().strip()
    operator = a.operator_token_file.read_text().strip() if a.operator_token_file else None
    session = 'smoke-' + uuid.uuid4().hex
    opener = urllib.request.build_opener(NoRedirect)
    results = []
    request_ids = []

    def call(name, path, body=None, key=token, expected=200, session_id=session, headers=None, check=None):
        h = {'Content-Type': 'application/json'}
        if key: h['Authorization'] = 'Bearer ' + key
        if session_id: h['X-Station-Session'] = session_id
        h.update(headers or {})
        req = urllib.request.Request(a.base_url.rstrip('/') + path,
            data=json.dumps(body).encode() if body is not None else None, headers=h)
        start = time.monotonic()
        try:
            try: response = opener.open(req, timeout=90)
            except urllib.error.HTTPError as e: response = e
            with response:
                status = response.code
                first = None
                parts = []
                while True:
                    line = response.readline()
                    if not line: break
                    if first is None: first = time.monotonic() - start
                    parts.append(line)
                    if sum(map(len, parts)) > 4 * 1024 * 1024: raise ValueError('response exceeded probe cap')
                raw = b''.join(parts)
                req_id = response.headers.get('X-Request-Id')
                if body is not None and status == 200 and req_id: request_ids.append(req_id)
                ok = status == expected and (check(raw) if check and status == 200 else True)
                result = {'name': name, 'status': status, 'ok': bool(ok), 'elapsed_ms': round((time.monotonic()-start)*1000), 'request_id': req_id}
                if body and body.get('stream'): result['first_line_ms'] = round(first*1000) if first else None
                results.append(result)
                print(json.dumps(result), flush=True)
                return json.loads(raw) if raw and not (body and body.get('stream')) else None
        except Exception as e:
            result = {'name': name, 'ok': False, 'error_class': type(e).__name__}
            results.append(result); print(json.dumps(result), flush=True)
            return None

    call('no credential', '/models', key=None, expected=401)
    call('invalid credential', '/models', key='invalid', expected=401)
    call('models', '/models', check=lambda b: bool(json.loads(b).get('data')))
    call('usage scope denied', '/usage', expected=403)
    call('catalog mutation scope denied', '/catalog/refresh', body={}, expected=403)
    call('Go requires session', '/chat/completions', body={'model':'go-glm','messages':[{'role':'user','content':'Reply OK'}]}, session_id=None, expected=400)
    call('Go GPT rejects chat endpoint', '/chat/completions', body={'model':'go-gpt','messages':[{'role':'user','content':'Reply OK'}]}, expected=400)
    call('unknown model', '/chat/completions', body={'model':'not-a-configured-model','messages':[{'role':'user','content':'Reply OK'}]}, expected=404)
    if a.live:
        chat = lambda model: {'model': model, 'messages':[{'role':'user','content':'Reply with the single word OK.'}], 'max_completion_tokens':256}
        for model in ['go-glm','openai-mini','gemini-flash','openrouter-auto','hf-llama','local-light']:
            body = chat(model)
            if model != 'openai-mini': body['max_tokens'] = body.pop('max_completion_tokens')
            call(model, '/chat/completions', body=body, check=lambda b: bool(json.loads(b).get('choices')))
        call('go-gpt', '/responses', body={'model':'go-gpt','input':'Reply with the single word OK.','max_output_tokens':512}, check=lambda b: json.loads(b).get('status') == 'completed')
        for model in ['go-haiku','anthropic-haiku']:
            call(model, '/messages', body={'model':model,'max_tokens':64,'messages':[{'role':'user','content':'Reply with the single word OK.'}]}, headers={'anthropic-version':'2023-06-01'}, check=lambda b: bool(json.loads(b).get('content')))
        for model in ['embed-local','embed-openai']:
            call(model, '/embeddings', body={'model':model,'input':'station gateway dogfood probe'}, check=lambda b: bool(json.loads(b).get('data',[{}])[0].get('embedding')))
        call('jev', '/systemone', body={'model':'jev','state':'The sky is blue.','questions':{'blue':{'type':'noul','instructions':'Is the sky blue?'}}}, check=lambda b: bool(json.loads(b).get('answers')))
        body=chat('go-glm');body['max_tokens']=body.pop('max_completion_tokens');body['stream']=True
        call('Go streaming', '/chat/completions', body=body, check=lambda b: b'data:' in b and b'[DONE]' in b)
        call('Go Responses streaming', '/responses', body={'model':'go-gpt','input':'Reply with the single word OK.','max_output_tokens':512,'stream':True}, check=lambda b: b'response.completed' in b)
        call('Go Messages streaming', '/messages', body={'model':'go-haiku','messages':[{'role':'user','content':'Reply with the single word OK.'}],'max_tokens':64,'stream':True}, headers={'anthropic-version':'2023-06-01'}, check=lambda b: b'message_stop' in b)
        schema={'type':'object','properties':{'value':{'type':'string'}},'required':['value'],'additionalProperties':False}
        function={'name':'connectivity_check','description':'Report the connectivity test value.','parameters':schema}
        call('Go chat tool call','/chat/completions',body={'model':'go-glm','messages':[{'role':'user','content':'Call connectivity_check with value OK.'}],'tools':[{'type':'function','function':function}],'tool_choice':{'type':'function','function':{'name':'connectivity_check'}},'max_tokens':512},check=lambda b: bool(json.loads(b).get('choices',[{}])[0].get('message',{}).get('tool_calls')))
        call('Go Responses tool call','/responses',body={'model':'go-gpt','input':'Call connectivity_check with value OK.','tools':[{'type':'function',**function}],'tool_choice':{'type':'function','name':'connectivity_check'},'max_output_tokens':512},check=lambda b: any(v.get('type')=='function_call' for v in json.loads(b).get('output',[])))
        call('Go Messages tool call','/messages',body={'model':'go-haiku','messages':[{'role':'user','content':'Call connectivity_check with value OK.'}],'tools':[{'name':'connectivity_check','description':'Report connectivity test value.','input_schema':schema}],'tool_choice':{'type':'tool','name':'connectivity_check'},'max_tokens':256},headers={'anthropic-version':'2023-06-01'},check=lambda b: any(v.get('type')=='tool_use' for v in json.loads(b).get('content',[])))
        if operator:
            time.sleep(1)  # async ledger flush
            usage=call('operator usage', '/usage?limit=1000', key=operator)
            entries=(usage or {}).get('entries') or []
            own=[e for e in entries if e.get('request_id') in request_ids]
            result={'name':'persisted attribution','ok':bool(request_ids) and set(request_ids)<=set(e['request_id'] for e in own) and all(e.get('verified') and e.get('conversation_id') for e in own),'records':len(own)}
            results.append(result);print(json.dumps(result),flush=True)
    if a.output: a.output.write_text(json.dumps({'session':session,'base_url':a.base_url,'results':results,'request_ids':request_ids},indent=2)+'\n')
    return 0 if all(r['ok'] for r in results) else 1

if __name__ == '__main__':
    raise SystemExit(main())
