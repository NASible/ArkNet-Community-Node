#!/usr/bin/env python3
"""Configure one isolated Jellyfin, approve its Node, and provision its owner."""
import argparse
import datetime
import subprocess
import sys
import urllib.parse
from ops import HERE, MACHINES, load_secrets, mint_core_admin, revoke_core_admin, save_secrets
from private_api import TLS, operator_headers, request, tunnel, wait
sys.path.insert(0, str(HERE.parent.parent))
from scripts import global_login  # noqa: E402

parser = argparse.ArgumentParser()
parser.add_argument('host', choices=[h for h,m in MACHINES.items() if m['role'] == 'node'])
args = parser.parse_args()
machine = MACHINES[args.host]
label = args.host[-1]
secrets = load_secrets()
local = secrets['machines'][args.host]
# Scoped, short-lived Core admin tokens (ARKSEC-005), minted on first use on the
# Core VM's shell and revoked when this run ends. Never stored or printed.
minted = {}


def admin(permission):
    if permission not in minted:
        minted[permission] = mint_core_admin('init-' + args.host, permission, ttl='1h')
    return {'Authorization': 'Bearer ' + minted[permission][0]}

core = 'https://core.nodenet.arknet.media'

with tunnel(machine, 8096) as jelly:
    _, public = request(jelly + '/System/Info/Public')
    username = 'owner-' + label
    if not public.get('StartupWizardCompleted'):
        request(jelly + '/Startup/Configuration', 'POST', {
            'UICulture': 'en-US', 'MetadataCountryCode': 'US',
            'PreferredMetadataLanguage': 'en', 'ServerName': 'NodeNet ' + label.upper()})
        # Jellyfin 12 initializes the first user when its startup GET is called.
        request(jelly + '/Startup/User')
        request(jelly + '/Startup/User', 'POST', {'Name': username, 'Password': local['jellyfin_password']})
        request(jelly + '/Startup/RemoteAccess', 'POST', {'EnableRemoteAccess': True, 'EnableAutomaticPortMapping': False})
        request(jelly + '/Startup/Complete', 'POST', {})
    authorization = 'MediaBrowser Client="NodeNet operator", Device="operator-mac", DeviceId="nodenet-private-pilot-' + label + '", Version="0.0.1-dev"'
    _, login = request(jelly + '/Users/AuthenticateByName', 'POST',
                       {'Username': username, 'Pw': local['jellyfin_password']}, {'Authorization': authorization})
    auth = {'Authorization': 'MediaBrowser Token="' + login['AccessToken'] + '"'}
    local['jellyfin_username'] = username
    local['jellyfin_user_id'] = login['User']['Id']
    local['jellyfin_access_token'] = login['AccessToken']
    save_secrets(secrets)
    _, libraries = request(jelly + '/Library/VirtualFolders', headers=auth)
    if not any(item.get('Name') == 'ArkNet' for item in libraries):
        query = urllib.parse.urlencode({'name': 'ArkNet', 'collectionType': 'movies', 'refreshLibrary': 'false'})
        request(jelly + '/Library/VirtualFolders?' + query, 'POST', headers=auth)
        request(jelly + '/Library/VirtualFolders/Paths?name=ArkNet&refreshLibrary=true', 'POST',
                {'Name': 'ArkNet', 'Path': '/srv/arknet/media'}, auth)
    _, keys = request(jelly + '/Auth/Keys', headers=auth)
    key = next((item['AccessToken'] for item in keys['Items'] if item['AppName'] == 'nodenet-agent'), None)
    if not key:
        request(jelly + '/Auth/Keys?app=nodenet-agent', 'POST', {}, auth)
        _, keys = request(jelly + '/Auth/Keys', headers=auth)
        key = next(item['AccessToken'] for item in keys['Items'] if item['AppName'] == 'nodenet-agent')
    local['jellyfin_api_key'] = key
    # Direct-play pilot: don't spend the small VM budget on transcoding.
    policy = dict(login['User'].get('Policy', {}))
    policy.update({'EnableAudioPlaybackTranscoding': False, 'EnableVideoPlaybackTranscoding': False,
                   'EnablePlaybackRemuxing': True, 'EnableRemoteAccess': True})
    request(jelly + '/Users/' + local['jellyfin_user_id'] + '/Policy', 'POST', policy, auth)
    _, libraries = request(jelly + '/Library/VirtualFolders', headers=auth)
    assert any('/srv/arknet/media' in item.get('Locations', []) for item in libraries)
    save_secrets(secrets)
    print('JELLYFIN_CONFIGURED_AND_LIBRARY_VERIFIED', args.host, username, flush=True)

subprocess.run([sys.executable, str(HERE / 'deploy-stack.py'), args.host], check=True, stdout=subprocess.DEVNULL)
try:
    with tunnel(machine, 8082) as agent, tunnel(machine, 8086) as member_listener:
        # Management routes require a Node operator session (Prompt 45B);
        # sign-in is a public route on the member listener (Prompt 45C).
        operator = operator_headers(args.host, member_listener)

        def enrolled():
            state = request(agent + '/api/v1/status', headers=operator)[1]
            return state if state.get('state') in ('ENROLLMENT_PENDING', 'CONNECTED') else None
        state = wait(args.host + ' enrollment', enrolled, 180)
        node_id = state['node_id']
        local['node_id'] = node_id
        save_secrets(secrets)
        if state['state'] != 'CONNECTED':
            _, decision = request(core + '/api/admin/v1/nodes/' + node_id + '/approve', 'POST', headers=admin('nodes.decide'))
            assert decision['enrollment_status'] == 'APPROVED'
        connected = wait(args.host + ' connected', lambda: (s if (s := request(agent + '/api/v1/status', headers=operator)[1]).get('state') == 'CONNECTED' else None), 180)
        assert connected['jellyfin_healthy'] is True
        peer = request(agent + '/api/v1/settings/peer-serving', headers=operator)[1]
        # Do not enable peer serving as a side effect of deployment.
        assert peer.get('enabled') is False
        owner = secrets['global_users']['user-' + label]
        if not owner.get('user_id'):
            _, created = request(core + '/api/admin/v1/users', 'POST',
                                 {'username': owner['username'], 'password': owner['password']}, admin('users.admin'))
            owner['user_id'] = created['user_id']
            save_secrets(secrets)
        _, login = request(core + '/api/identity/v1/sessions', 'POST', {'username': owner['username'], 'password': owner['password']})
        session = {'Authorization': 'Bearer ' + login['token']}
        if not owner.get('member_id'):
            expires = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)).strftime('%Y-%m-%dT%H:%M:%SZ')
            _, invite = request(agent + '/api/v1/invitations', 'POST', {'role': 'MEMBER', 'expires_at': expires, 'max_uses': 1}, operator)
            _, accepted = request(core + '/api/identity/v1/invitations/accept', 'POST', {'secret': invite['secret']}, session)
            assert accepted['node_id'] == node_id
            owner['member_id'] = accepted['member_id']
            owner['node_id'] = node_id
            save_secrets(secrets)
        request(core + '/api/identity/v1/home-node', 'PUT', {'node_id': node_id}, session)
        # Verify the Core-bound pairwise login on the public member host (Prompt 46B):
        # the password goes only to Core's page; the Node verifies Core's assertion.
        member = 'https://member-' + args.host[-1] + '.nodenet.arknet.media'
        node_session, _ = global_login.login(global_login.Browser(TLS), member, member + '/login/callback',
                                             owner['username'], owner['password'])
        assert node_session.get('token') and node_session['member_id'] == owner['member_id']
        request(core + '/api/identity/v1/session', 'DELETE', headers=session, expected=(200, 204))
        print('NODE_CONNECTED_OWNER_MEMBERSHIP_LOGIN_VERIFIED', args.host, owner['username'], flush=True)
finally:
    for _, token_id in minted.values():
        revoke_core_admin(token_id)

subprocess.run([sys.executable, str(HERE / 'activate-ingress.py'), args.host], check=True)
