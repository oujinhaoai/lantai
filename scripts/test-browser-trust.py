#!/usr/bin/env python3
"""Give one browser test process an explicit CA in a disposable NSS home."""
import argparse
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--ca-file', type=Path, required=True)
    parser.add_argument('--executable', type=Path, required=True)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command or not args.ca_file.is_file() or not args.executable.is_file():
        raise SystemExit('Explicit test CA, installed browser and command are required')
    home = Path(tempfile.mkdtemp(prefix='lantai-browser-trust-'))
    try:
        database = home / '.pki/nssdb'
        database.mkdir(parents=True, mode=0o700)
        subprocess.run(['certutil', '-N', '-d', 'sql:' + str(database), '--empty-password'],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        subprocess.run(['certutil', '-A', '-d', 'sql:' + str(database), '-n',
                        'Lantai synthetic browser CA', '-t', 'C,,', '-i', str(args.ca_file)],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        env = dict(os.environ, HOME=str(home),
                   LANTAI_TEST_NSS_DATABASE='sql:' + str(database),
                   LANTAI_TEST_NSS_CA_SHA256=hashlib.sha256(args.ca_file.read_bytes()).hexdigest(),
                   LANTAI_WEB_CHROMIUM_EXECUTABLE=str(args.executable),
                   NODE_EXTRA_CA_CERTS=str(args.ca_file))
        result = subprocess.run(command, env=env)
        raise SystemExit(result.returncode)
    finally:
        shutil.rmtree(home)


if __name__ == '__main__':
    main()
