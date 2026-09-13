#!/usr/bin/env python3
"""Package only the native bytes verified by the pinned CPA suite smoke test."""
import hashlib
import json
from pathlib import Path
import stat
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TAG = 'quota-cache-preview-2'
VERSIONS = {'quota-cache':'0.1.0','account-health-pushover':'0.4.2','reset-priority':'0.1.6','auto-baseline':'0.1.3','token-usage':'0.1.2'}
LIBRARIES = {'quota-cache':'dist/quota-cache.so','account-health-pushover':'dist/account-health-pushover.so','reset-priority':'reset-priority.so','auto-baseline':'auto-baseline.so','token-usage':'dist/token-usage.so'}

def digest(raw): return hashlib.sha256(raw).hexdigest()

def main():
    evidence = json.loads((ROOT/'dist/quota-preview-evidence.json').read_text())
    assert evidence['image'] == 'eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b'
    out = ROOT/'dist'/TAG
    out.mkdir(parents=True,exist_ok=True)
    registry = json.loads((ROOT/'registry.json').read_text())
    entries = {entry['id']:entry for entry in registry['plugins']}
    entries['quota-cache'] = {'id':'quota-cache','name':'Quota Cache','description':'Serialized weekly quota polling with a persistent cache and provider-wide 429 cooldowns. Preview: Linux amd64 only.','author':'NoorChasib','license':'MIT'}
    sums = []
    for plugin,version in VERSIONS.items():
        library = (ROOT/'plugins'/plugin/LIBRARIES[plugin]).read_bytes()
        assert evidence['libraries'][plugin] == {'sha256':digest(library),'version':version}, 'library must match pinned CPA verification'
        assert library[:4] == b'\x7fELF', 'expected Linux ELF'
        archive_name = f'{plugin}_{version}_linux_amd64.zip'
        archive = out/archive_name
        members = [(plugin+'.so',library,0o755),('LICENSE',(ROOT/'plugins'/plugin/'LICENSE').read_bytes(),0o644)]
        if plugin == 'token-usage':
            members.append(('THIRD-PARTY-NOTICES.txt',(ROOT/'plugins/token-usage/docs/third-party-notices.txt').read_bytes(),0o644))
        with zipfile.ZipFile(archive,'w',compression=zipfile.ZIP_DEFLATED) as z:
            for name,raw,mode in members:
                info = zipfile.ZipInfo(name,date_time=(2026,9,13,0,0,0))
                info.create_system=3
                info.external_attr=(stat.S_IFREG|mode)<<16
                z.writestr(info,raw,compress_type=zipfile.ZIP_DEFLATED)
        with zipfile.ZipFile(archive) as z:
            assert z.namelist() == [name for name,_,_ in members]
            assert z.read(plugin+'.so') == library
            assert z.testzip() is None
        raw=archive.read_bytes()
        entries[plugin].update({'version':version,'repository':'https://github.com/NoorChasib/cpa-plugins',
            'homepage':f'https://github.com/NoorChasib/cpa-plugins/tree/main/plugins/{plugin}',
            'install':{'type':'direct','artifacts':[{'goos':'linux','goarch':'amd64','url':f'https://github.com/NoorChasib/cpa-plugins/releases/download/{TAG}/{archive_name}','sha256':digest(raw),'size':len(raw)}]}})
        sums.append(f'{digest(raw)}  {archive_name}')
    registry['plugins'] = [entries[key] for key in sorted(entries)]
    raw=(json.dumps(registry,indent=2)+'\n').encode()
    (out/'registry.json').write_bytes(raw)
    (ROOT/'preview').mkdir(exist_ok=True)
    (ROOT/'preview/registry.json').write_bytes(raw)
    (ROOT/'registry.json').write_bytes(raw)
    sums.append(f'{digest(raw)}  registry.json')
    evidence_raw=(json.dumps(evidence,indent=2)+'\n').encode()
    (out/'verification.json').write_bytes(evidence_raw)
    sums.append(f'{digest(evidence_raw)}  verification.json')
    (out/'checksums.txt').write_text('\n'.join(sorted(sums))+'\n')
    print('Packaged verified Linux amd64 preview:', ', '.join(f'{p} {v}' for p,v in VERSIONS.items()))

if __name__=='__main__': main()
