"""Fail-closed identity proof for previously hash-verified Docker/OCI archives."""
import hashlib
import json
from pathlib import PurePosixPath
import re
import tarfile

MANIFEST = 'application/vnd.oci.image.manifest.v1+json'
INDEX = 'application/vnd.oci.image.index.v1+json'
CONFIG = 'application/vnd.oci.image.config.v1+json'


def archive_identity(archive, expected_config, expected_tag):
    """Caller must verify the whole historical archive before calling or loading it."""
    with tarfile.open(archive, 'r:gz') as tar:
        members = {}
        for member in tar.getmembers():
            path = PurePosixPath(member.name)
            if (path.is_absolute() or '..' in path.parts or member.name in members
                    or not (member.isfile() or member.isdir())):
                raise RuntimeError('unsafe image archive member')
            members[member.name] = member

        def read(name):
            member = members.get(name)
            if not member or not member.isfile() or member.size > 1024 * 1024:
                raise RuntimeError('missing or oversized image metadata')
            return tar.extractfile(member).read()

        def blob(desc):
            digest = desc.get('digest', '')
            if not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
                raise RuntimeError('invalid image descriptor digest')
            raw = read('blobs/sha256/' + digest[7:])
            if (len(raw) != desc.get('size') or
                    'sha256:' + hashlib.sha256(raw).hexdigest() != digest):
                raise RuntimeError('image descriptor bytes mismatch')
            return json.loads(raw)

        root = json.loads(read('index.json'))
        descriptors = {}
        node = root
        for depth in range(5):
            if node.get('schemaVersion') != 2 or len(node.get('manifests', [])) != 1:
                raise RuntimeError('ambiguous image archive platform selection')
            desc = node['manifests'][0]
            media = desc.get('mediaType')
            if media not in (MANIFEST, INDEX) or desc.get('digest') in descriptors:
                raise RuntimeError('unsupported or cyclic image descriptor')
            node = blob(desc)
            if node.get('mediaType') != media or node.get('schemaVersion') != 2:
                raise RuntimeError('image descriptor media mismatch')
            descriptors[desc['digest']] = {k: desc[k] for k in ('digest', 'size', 'mediaType')}
            if media == MANIFEST:
                break
        else:
            raise RuntimeError('image descriptor nesting exceeds limit')
        config_desc = node.get('config', {})
        if config_desc.get('digest') != expected_config or config_desc.get('mediaType') != CONFIG:
            raise RuntimeError('image configuration differs from original pinned identity')
        config = blob(config_desc)
        if config.get('os') != 'linux' or config.get('architecture') != 'amd64':
            raise RuntimeError('image configuration platform mismatch')
        legacy = json.loads(read('manifest.json'))
        if (len(legacy) != 1 or legacy[0].get('Config') != 'blobs/sha256/' + expected_config[7:]
                or legacy[0].get('RepoTags') != [expected_tag]
                or legacy[0].get('Layers') != ['blobs/sha256/' + x['digest'][7:] for x in node.get('layers', [])]):
            raise RuntimeError('Docker and OCI archive identities disagree')
        return {'config_digest': expected_config, 'descriptors': descriptors,
                'labels': config.get('config', {}).get('Labels', {}), 'os': 'linux', 'architecture': 'amd64'}


def verified_runtime_id(value, proof):
    """Accept only exact config ID or an archive-proven content-addressed descriptor."""
    if (value.get('Os') != proof['os'] or value.get('Architecture') != proof['architecture']
            or value.get('Config', {}).get('Labels', {}) != proof['labels']):
        raise RuntimeError('loaded image configuration metadata mismatch')
    actual = value.get('Id')
    if actual == proof['config_digest']:
        return actual
    expected = proof['descriptors'].get(actual)
    descriptor = value.get('Descriptor') or {}
    if not expected or any(descriptor.get(k) != v for k, v in expected.items()):
        raise RuntimeError('loaded image has no verified archive descriptor relationship')
    return actual
