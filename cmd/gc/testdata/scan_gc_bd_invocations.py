#!/usr/bin/env python3
# Scans shipped sources for gc bd invocations and prints one JSON object per
# distinct argv ({"argv": [...], "src": "repo:path:line"}), with $VARS,
# {{templates}}, <placeholders> and $(...) normalized to X. It drops prose
# (tokens ending in . , :), gc's own --rig/--city, and a value flag whose value
# was cut off with the line gets X.
#
# usage (from the gascity repo root):
#   python3 cmd/gc/testdata/scan_gc_bd_invocations.py <verbs.json> \
#     "gascity|$PWD|" "gascity-packs|<path>|origin/main" "workflows|<path>|" \
#     > cmd/gc/testdata/gc_bd_shipped_invocations.jsonl
import re, shlex, json, subprocess, sys, os
def load_inventory(d):
    verbs, value_flags, parents = set(), set(), set()
    for f in sorted(os.listdir(d)):
        if not f.endswith('.json'): continue
        for c in json.load(open(os.path.join(d, f)))['commands']:
            p = c['path']
            if p:
                words = p.split(' ')
                verbs.add(words[0])
                if len(words) > 1: parents.add(words[0])
                if len(words) == 1: verbs.update(c.get('aliases') or [])
            for fl in (c.get('local') or []) + (c.get('persistent') or []):
                if not fl.get('no_opt_def_val'):
                    value_flags.add('--' + fl['name'])
                    if fl.get('shorthand'): value_flags.add('-' + fl['shorthand'])
    # Verbs that run on their own as well as having subcommands.
    return verbs, value_flags, parents - {'comments', 'state', 'human', 'graph'}
verbs, value_flags, parents = load_inventory(sys.argv[1])
out=[]
def files(repo, ref):
    if ref:
        names=subprocess.run(['git','-C',repo,'ls-tree','-r','--name-only',ref],capture_output=True,text=True).stdout.split('\n')
    else:
        names=subprocess.run(['git','-C',repo,'ls-files'],capture_output=True,text=True).stdout.split('\n')
    for n in names:
        if not n or not re.search(r'\.(sh|bash|toml|md|tmpl|py|txt|yaml|yml|json)$', n): continue
        if re.search(r'(_test\.(go|py|sh)$|/tests?/|testdata|fixtures|test_[^/]*\.(py|sh)$)', n): continue
        if label == 'gascity' and not re.match(r'(internal/bootstrap/packs/|examples/|cmd/gc/.*\.(md|tmpl)$)', n): continue
        if ref:
            data=subprocess.run(['git','-C',repo,'show',f'{ref}:{n}'],capture_output=True).stdout.decode('utf-8','replace')
        else:
            try: data=open(os.path.join(repo,n),errors='replace').read()
            except: continue
        yield n, data
term=re.compile(r'(\|\||&&|\||;|`|\)\s*$|\s>|\s2>|\s<|#\s)')
def norm(seg):
    seg=re.sub(r'\$\([^()]*\)','X',seg)
    seg=re.sub(r'\$\{[^}]*\}','X',seg)
    seg=re.sub(r'\$[A-Za-z_][A-Za-z0-9_]*','X',seg)
    seg=re.sub(r'\{\{[^}]*\}\}','X',seg)
    seg=re.sub(r'<[^<>\s][^<>]*>','X',seg)
    return seg
for spec in sys.argv[2:]:
    label, repo, ref = spec.split('|')
    for name, data in files(repo, ref or None):
        data=data.replace('\\\n',' ')
        for ln, line in enumerate(data.split('\n'),1):
            for m in re.finditer(r'(?<![\w-])gc bd\s+', line):
                rest=line[m.end():]
                cut=term.search(rest)
                if cut: rest=rest[:cut.start()]
                rest=norm(rest).strip().rstrip('`').strip()
                rest=re.sub(r'["\']?\s*$','',rest) if rest.count('"')%2 else rest
                try: toks=shlex.split(rest)
                except ValueError: continue
                # drop gc-level scope flags
                t2=[];i=0
                while i<len(toks):
                    if toks[i] in ('--rig','--city'): i+=2; continue
                    if toks[i].startswith('--rig=') or toks[i].startswith('--city='): i+=1; continue
                    t2.append(toks[i]); i+=1
                toks=[t for t in t2 if t not in ('...','…')]
                first=next((t for t in toks if not t.startswith('-')),None)
                if not toks or first is None or first not in verbs: continue
                if any(c in ''.join(toks) for c in '()[]{}'): continue
                if any(t.endswith(('.', ',', ':')) or '. ' in t for t in toks): continue  # prose
                if toks[-1] in value_flags: toks.append('X')  # the value was cut off with the line
                # Placeholders where bd needs a shape: --set-metadata k=v, --metadata {...}.
                for j in range(len(toks)-1):
                    if toks[j] == '--set-metadata' and '=' not in toks[j+1]: toks[j+1] = 'k=' + toks[j+1]
                    if toks[j] == '--metadata' and not toks[j+1].startswith(('{', '@')): toks[j+1] = '{}'
                if toks in (['sql'], ['import']): continue  # prose: "gc bd sql reads", "gc bd import"
                pos=[t for t in toks if not t.startswith('-')]
                if pos and pos[0] in parents and len(pos) == 1: continue  # "gc bd mol" in prose
                out.append({'argv':toks,'src':f'{label}:{name}:{ln}'})
seen={}
for o in out:
    k=json.dumps(o['argv'])
    seen.setdefault(k,o)
for k in sorted(seen):
    print(json.dumps(seen[k]))
