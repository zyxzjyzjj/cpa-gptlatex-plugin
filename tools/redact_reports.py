"""Redact real credentials accidentally recorded in the HAR analysis reports."""
import re, glob, os

COOKIES = [
    'cf_clearance', 'prism_session_token', 'prism_oai_access_token',
    'prism_oai_refresh_token', 'prism_oai_pending_access_token',
    'prism_oai_earliest_refresh_at', '__cf_bm', '__cflb', '_cfuvid',
    'oai-sc', 'YSWEET_OFFLINE_KEY', 'oai-did', 'prism-did',
    'prism_openai_transfer_proof', 'prism_openai_oauth_state_binding',
]

name_re = re.compile(r'\b(' + '|'.join(re.escape(c) for c in COOKIES) + r')=([A-Za-z0-9_.\-]{8,})')
jwt_re = re.compile(r'eyJ[A-Za-z0-9_\-]{6,}\.[A-Za-z0-9_\-]{6,}\.[A-Za-z0-9_\-]{6,}')
fernet_re = re.compile(r'0?gAAAAAB[A-Za-z0-9_\-]{10,}')

total = 0
targets = sorted(set(glob.glob(os.path.join('tools', '*.md'))))
for path in targets:
    with open(path, encoding='utf-8', errors='replace') as fh:
        src = fh.read()
    out = name_re.sub(lambda m: '%s=<REDACTED>' % m.group(1), src)
    out = jwt_re.sub('<JWT-REDACTED>', out)
    out = fernet_re.sub('gAAAAAB…', out)
    n = sum(1 for a, b in zip(src.split('\n'), out.split('\n')) if a != b)
    if out != src:
        with open(path, 'w', encoding='utf-8', newline='') as fh:
            fh.write(out)
        hits = len(name_re.findall(src)) + len(jwt_re.findall(src)) + len(fernet_re.findall(src))
        total += hits
        print('redacted %-24s %d credential(s) on %d line(s)' % (os.path.basename(path), hits, n))
print('\ntotal redacted: %d' % total)
