#!/usr/bin/env python3
"""Throwaway runner; only removes its UUID-named container and image."""
import argparse
import json
import pathlib
import subprocess
import sys
import uuid

ROOT = pathlib.Path(__file__).resolve().parent

def run(args, **kw):
    return subprocess.run(args, cwd=ROOT, text=True, capture_output=True, **kw)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--mib', type=int, default=32)
    parser.add_argument('--reps', type=int, default=3)
    args = parser.parse_args()
    name = 'prompt48e-community-relay-' + uuid.uuid4().hex[:12]
    image = name + ':throwaway'
    report = None
    status = 1
    commands = []
    try:
        cmd = ['docker', 'build', '-t', image, str(ROOT)]
        commands.append(cmd)
        built = run(cmd)
        (ROOT / 'build.log').write_text(built.stdout + built.stderr)
        if built.returncode:
            raise RuntimeError('Docker build/test failed; see build.log')
        cmd = ['docker', 'create', '--name', name, '--network', 'none',
               '--cap-drop', 'ALL', '--read-only', '--security-opt',
               'no-new-privileges:true', image, '-mib', str(args.mib), '-reps', str(args.reps)]
        commands.append(cmd)
        created = run(cmd)
        if created.returncode:
            raise RuntimeError('Docker create failed: ' + created.stderr)
        config = json.loads(run(['docker', 'inspect', name]).stdout)[0]
        host = config['HostConfig']
        isolation = {'network': host['NetworkMode'], 'cap_drop': host['CapDrop'],
                     'read_only': host['ReadonlyRootfs'], 'user': config['Config']['User'],
                     'security_opt': host['SecurityOpt'], 'published_ports': host['PortBindings'],
                     'mounts': config['Mounts'],
                     'image_platform': json.loads(run(['docker', 'image', 'inspect', image]).stdout)[0]['Architecture']}
        assert isolation['network'] == 'none' and isolation['cap_drop'] == ['ALL']
        assert isolation['read_only'] and isolation['user'] == '65532:65532'
        assert not isolation['published_ports'] and not isolation['mounts']
        commands.append(['docker', 'start', '-a', name])
        completed = run(commands[-1])
        (ROOT / 'checks.log').write_text(completed.stderr)
        print(completed.stderr, end='', file=sys.stderr)
        report = json.loads(completed.stdout)
        report['docker_runtime_verified'] = isolation
        state = json.loads(run(['docker', 'inspect', name]).stdout)[0]['State']
        report['container_exit_code'] = state['ExitCode']
        status = state['ExitCode']
        # Never persist TLS certificates, signed capabilities, or private keys.
        serialized = json.dumps(report)
        for forbidden in ('SPIKE_GRANT_CANARY_DO_NOT_DISCLOSE', 'PRIVATE KEY', '"Sig"', '"Body"'):
            assert forbidden not in serialized
    finally:
        run(['docker', 'rm', '-f', name])
        run(['docker', 'image', 'rm', image])
        container_gone = run(['docker', 'inspect', name]).returncode != 0
        image_gone = run(['docker', 'image', 'inspect', image]).returncode != 0
        (ROOT / 'commands.json').write_text(json.dumps(commands, indent=2) + '\n')
        if report is not None:
            report['scoped_cleanup_verified'] = {'container_absent': container_gone, 'image_absent': image_gone}
            (ROOT / 'result.json').write_text(json.dumps(report, indent=2) + '\n')
        if not container_gone or not image_gone:
            raise RuntimeError('Own resource cleanup failed')
    if report is not None:
        print(json.dumps({'result': str(ROOT / 'result.json'), 'medians': report['medians'],
                          'protocol_checks_pass': report['protocol_checks_pass'],
                          'deployment_verdict': report['deployment_verdict']}, indent=2))
    return status

if __name__ == '__main__':
    sys.exit(main())
