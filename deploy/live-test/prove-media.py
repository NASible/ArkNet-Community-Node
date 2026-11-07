#!/usr/bin/env python3
"""Real generated sample: Core web seed -> each Agent/worker -> Jellyfin stream."""
import base64
import hashlib
import json
from pathlib import Path
import secrets
import shlex
import subprocess
import urllib.parse
import urllib.request
from ops import HERE, MACHINES, load_secrets, put, ssh
from private_api import TLS, operator_headers, request, wait

record_file = HERE / 'sample-proof.json'
if record_file.exists():
    sample = json.loads(record_file.read_text())
else:
    object_id = 'media_' + secrets.token_hex(32)
    directory = Path('/Users/macbook/.hermes/cache/scratch/nodenet-generated-sample')
    directory.mkdir(mode=0o700, exist_ok=True)
    source = directory / object_id
    subprocess.run(['ffmpeg', '-hide_banner', '-loglevel', 'error', '-y', '-f', 'lavfi', '-i',
                    'testsrc2=size=640x360:rate=24', '-f', 'lavfi', '-i', 'sine=frequency=440:sample_rate=48000',
                    '-t', '8', '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p',
                    '-c:a', 'aac', '-movflags', '+faststart', '-f', 'mp4', str(source)], check=True)
    payload = source.read_bytes()
    put('nodenet-node-a', '/srv/arknet/test-source/' + object_id, payload)
    code = '''import libtorrent as lt,json,base64
from pathlib import Path
p=Path('/work/OBJECT_ID')
f=lt.file_storage(); f.add_file(p.name,p.stat().st_size)
t=lt.create_torrent(f,256*1024,lt.create_torrent.v2_only)
t.set_priv(True); t.add_tracker('https://tracker.nodenet.arknet.media/announce')
lt.set_piece_hashes(t,str(p.parent)); data=lt.bencode(t.generate()); info=lt.torrent_info(data)
assert info.priv() and info.info_hashes().has_v2() and not info.info_hashes().has_v1()
print(json.dumps({'info_hash_v2':str(info.info_hashes().v2),'torrent':base64.b64encode(data).decode()}))
'''.replace('OBJECT_ID', object_id)
    image = ssh('nodenet-node-a', 'sudo docker inspect nodenet-node-a-arknet-transfer-1 --format "{{.Config.Image}}"').decode().strip()
    result = json.loads(ssh('nodenet-node-a', 'sudo docker run --rm --network none --entrypoint python3 -v /srv/arknet/test-source:/work:ro ' + image + ' -c ' + shlex.quote(code)))
    # Remove the temporary Node-side source before selecting the media. Only Core
    # has the payload when downloads begin; never pre-populate finalized media.
    ssh('nodenet-node-a', 'sudo python3 -c ' + shlex.quote("from pathlib import Path; Path('/srv/arknet/test-source/" + object_id + "').unlink()"))
    put('nodenet-core', '/srv/arknet/web-seed/' + object_id, payload)
    put('nodenet-core', '/srv/arknet/web-seed/' + object_id + '.torrent', base64.b64decode(result['torrent']))
    statement = ("INSERT INTO media_objects(public_id,content_type,title,size,state,info_hash_v2) VALUES ('" + object_id +
                 "','VIDEO','NodeNet generated playback test'," + str(len(payload)) + ",'ACTIVE','" + result['info_hash_v2'] + "');")
    ssh('nodenet-core', 'sudo docker exec -i nodenet-core-postgres-1 psql -U arknet -d arknet_core -v ON_ERROR_STOP=1', statement.encode())
    sample = {'object_id': object_id, 'bytes': len(payload), 'sha256': hashlib.sha256(payload).hexdigest(),
              'info_hash_v2': result['info_hash_v2'], 'generated_test_media': True, 'nodes': {}}
    record_file.write_text(json.dumps(sample, indent=2) + '\n')

secrets_state = load_secrets()
for host, machine in MACHINES.items():
    if machine['role'] != 'node':
        continue
    base = 'https://node-' + host[-1] + '.nodenet.arknet.media'
    operator = operator_headers(host)  # Node management requires an operator session (Prompt 45B)
    def catalog_item():
        return next((item for item in request(base + '/api/v1/media', headers=operator)[1]['objects'] if item['id'] == sample['object_id']), None)
    item = wait(host + ' signed catalog', catalog_item, 120)
    if item['local_state'] == 'NOT_PRESENT':
        request(base + '/api/v1/media/' + sample['object_id'] + '/selection', 'POST', {}, operator)
    # Exercise the supported explicit operator Start action. This proof does
    # not claim that queued selections begin automatically.
    if catalog_item()['local_state'] != 'AVAILABLE':
        request(base + '/api/v1/media/' + sample['object_id'] + '/transfer/start', 'POST', {}, operator)
    sample['start_trigger'] = 'operator transfer/start action'
    wait(host + ' real download AVAILABLE', lambda: (i if (i := catalog_item()) and i['local_state'] == 'AVAILABLE' else None), 180)
    verification = ssh(host, 'sudo python3 -c ' + shlex.quote("import hashlib,json; from pathlib import Path; p=Path('/srv/arknet/media/" + sample['object_id'] + ".mp4'); print(json.dumps({'sha256':hashlib.sha256(p.read_bytes()).hexdigest(),'bytes':p.stat().st_size}))"))
    finalized = json.loads(verification)
    assert finalized['sha256'] == sample['sha256'] and finalized['bytes'] == sample['bytes']
    jelly = 'https://jellyfin-' + host[-1] + '.nodenet.arknet.media'
    token = secrets_state['machines'][host]['jellyfin_access_token']
    auth = {'Authorization': 'MediaBrowser Token="' + token + '"'}
    # Indexing must be triggered by the product finalizer; don't manually refresh.
    # Jellyfin groups the generated samples (same "media_<hex>" naming) into one
    # Movie with several versions, so match the exact file among its media
    # sources and stream that source, not whichever version is the primary.
    def indexed():
        items = request(jelly + '/Items?Recursive=true&IncludeItemTypes=Movie&Fields=Path,MediaSources', headers=auth)[1]['Items']
        return next(((item, source) for item in items for source in item.get('MediaSources') or []
                     if source.get('Path', '').endswith('/' + sample['object_id'] + '.mp4')), None)
    movie, media_source = wait(host + ' Agent-triggered Jellyfin indexing', indexed, 120)
    stream_url = (jelly + '/Videos/' + movie['Id'] + '/stream.mp4?Static=true&MediaSourceId=' +
                  urllib.parse.quote(media_source['Id']))
    with urllib.request.urlopen(urllib.request.Request(stream_url, headers=auth), context=TLS, timeout=60) as response:
        delivered = response.read()
        assert response.status == 200
    assert len(delivered) == sample['bytes'] and hashlib.sha256(delivered).hexdigest() == sample['sha256']
    sample['nodes'][host] = {'available': True, 'finalized_sha256_matches': True,
                            'jellyfin_indexed': True, 'direct_stream_sha256_matches': True}
    record_file.write_text(json.dumps(sample, indent=2) + '\n')
    print('REAL_TRANSFER_AND_JELLYFIN_STREAM_PASS', host, sample['bytes'], flush=True)
print('ALL_THREE_NODE_MEDIA_PROOF_PASS', flush=True)
