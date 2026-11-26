"""Synthetic archive identity chains; no Docker execution or network."""
import copy
import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
sys.path.insert(0, str(Path(__file__).parents[1]))
import release_image_identity as identity


class ImageIdentity(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / 'image.tar.gz'
        self.blobs = {}
        self.tag = 'synthetic:test'
        self.config = {'os': 'linux', 'architecture': 'amd64', 'config': {'Labels': {'version': 'test'}}}
        self.config_desc = self.add_blob(self.config, identity.CONFIG)
        self.manifest = {'schemaVersion': 2, 'mediaType': identity.MANIFEST, 'config': self.config_desc, 'layers': []}
        self.desc = self.add_blob(self.manifest, identity.MANIFEST)
        self.index = {'schemaVersion': 2, 'mediaType': identity.INDEX, 'manifests': [self.desc]}
        self.legacy = [{'Config': 'blobs/sha256/' + self.config_desc['digest'][7:], 'RepoTags': [self.tag], 'Layers': []}]
        self.value = {'Id': self.desc['digest'], 'Descriptor': self.desc.copy(), 'Os': 'linux', 'Architecture': 'amd64', 'Config': self.config['config']}

    def add_blob(self, obj, media):
        raw = json.dumps(obj).encode()
        digest = hashlib.sha256(raw).hexdigest()
        self.blobs['blobs/sha256/' + digest] = raw
        return {'digest': 'sha256:' + digest, 'size': len(raw), 'mediaType': media}

    def write(self, extra=None):
        with tarfile.open(self.path, 'w:gz') as tar:
            for name, raw in {**self.blobs, 'index.json': json.dumps(self.index).encode(), 'manifest.json': json.dumps(self.legacy).encode()}.items():
                m = tarfile.TarInfo(name); m.size = len(raw); tar.addfile(m, io.BytesIO(raw))
            if extra: tar.addfile(extra)

    def proof(self):
        self.write()
        return identity.archive_identity(self.path, self.config_desc['digest'], self.tag)

    def test_config_and_manifest_runtime_ids(self):
        p = self.proof()
        self.assertEqual(identity.verified_runtime_id(self.value, p), self.desc['digest'])
        v = {**self.value, 'Id': self.config_desc['digest']}; v.pop('Descriptor')
        self.assertEqual(identity.verified_runtime_id(v, p), self.config_desc['digest'])

    def test_verified_single_child_index_runtime(self):
        nested = self.add_blob(self.index, identity.INDEX)
        self.index = {'schemaVersion': 2, 'mediaType': identity.INDEX, 'manifests': [nested]}
        v = {**self.value, 'Id': nested['digest'], 'Descriptor': nested}
        self.assertEqual(identity.verified_runtime_id(v, self.proof()), nested['digest'])

    def test_unknown_id_missing_or_mismatched_descriptor_rejected(self):
        p = self.proof()
        for change in ({'Id': 'sha256:' + 'a'*64}, {'Descriptor': {}}, {'Descriptor': {**self.desc, 'size': 1}}, {'Descriptor': {**self.desc, 'mediaType': identity.INDEX}}):
            with self.subTest(change=change), self.assertRaises(RuntimeError): identity.verified_runtime_id({**self.value, **change}, p)

    def test_wrong_loaded_platform_or_labels_rejected(self):
        p = self.proof()
        for change in ({'Os': 'windows'}, {'Architecture': 'arm64'}, {'Config': {'Labels': {'version': 'wrong'}}}):
            with self.subTest(change=change), self.assertRaises(RuntimeError): identity.verified_runtime_id({**self.value, **change}, p)

    def test_tampered_blob_and_size_rejected(self):
        key = 'blobs/sha256/' + self.desc['digest'][7:]
        self.blobs[key] += b' '
        with self.assertRaises(RuntimeError): self.proof()

    def test_wrong_config_tag_layers_or_multiplatform_rejected(self):
        for change in ('config', 'tag', 'layers', 'multiple'):
            old = copy.deepcopy((self.legacy, self.index))
            if change == 'config': self.legacy[0]['Config'] = 'blobs/sha256/' + '0'*64
            if change == 'tag': self.legacy[0]['RepoTags'] = ['other:test']
            if change == 'layers': self.legacy[0]['Layers'] = ['other']
            if change == 'multiple': self.index['manifests'].append(self.desc)
            with self.subTest(change=change), self.assertRaises(RuntimeError): self.proof()
            self.legacy, self.index = old

    def test_duplicate_path_and_symlink_rejected(self):
        for name, kind in [('index.json', tarfile.REGTYPE), ('link', tarfile.SYMTYPE), ('../escape', tarfile.REGTYPE)]:
            m = tarfile.TarInfo(name); m.type = kind
            self.write(m)
            with self.subTest(name=name), self.assertRaises(RuntimeError): identity.archive_identity(self.path, self.config_desc['digest'], self.tag)

    def test_pinned_config_cannot_be_replaced(self):
        self.write()
        with self.assertRaises(RuntimeError): identity.archive_identity(self.path, 'sha256:' + '0'*64, self.tag)


    def delivery(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location('delivery_full_baseline_test', Path(__file__).parents[1] / 'test-release-delivery.py')
        module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        return module

    def test_full_verified_baseline_normalises_core_and_gateway(self):
        module = self.delivery()
        self.write()
        for kind in ('core_image', 'caddy_image'):
            with self.subTest(kind=kind):
                old = {'source_commit': 'old', kind: {'id': self.config_desc['digest'], 'tag': self.tag}}
                self.assertEqual(module.register_baseline_runtime_id(self.path, old, kind, self.value), self.desc['digest'])
                self.assertEqual(module.runtime_id(old, kind), self.desc['digest'])
                self.assertEqual(old[kind]['id'], self.config_desc['digest'])

    def test_full_baseline_does_not_need_legacy_proof_if_runtime_id_already_exact(self):
        module = self.delivery()
        # A recent release may pin an OCI descriptor rather than config ID.
        old = {'source_commit': 'recent', 'core_image': {'id': self.desc['digest'], 'tag': self.tag}}
        self.assertEqual(module.register_baseline_runtime_id(self.path, old, 'core_image', self.value), self.desc['digest'])
        self.assertFalse(module.VERIFIED_RUNTIME_IDS)

    def test_full_baseline_rejects_unknown_wrong_platform_labels_and_tampered_archive(self):
        for change in ({'Id': 'sha256:'+'a'*64}, {'Descriptor': {}}, {'Os': 'windows'}, {'Architecture': 'arm64'}, {'Config': {'Labels': {'version': 'wrong'}}}):
            with self.subTest(change=change):
                module = self.delivery(); self.write()
                old = {'source_commit': 'old', 'core_image': {'id': self.config_desc['digest'], 'tag': self.tag}}
                with self.assertRaises(RuntimeError): module.register_baseline_runtime_id(self.path, old, 'core_image', {**self.value, **change})
                self.assertFalse(module.VERIFIED_RUNTIME_IDS)
        module = self.delivery(); key = 'blobs/sha256/' + self.desc['digest'][7:]; self.blobs[key] += b' '; self.write()
        with self.assertRaises(RuntimeError): module.register_baseline_runtime_id(self.path, old, 'core_image', self.value)
        self.assertFalse(module.VERIFIED_RUNTIME_IDS)

    def test_candidate_id_mismatch_never_gets_baseline_identity_proof(self):
        from unittest.mock import patch
        module = self.delivery()
        current = {'version': 'v0.1.0-test', 'source_commit': 'current', 'core_image': {'id': self.config_desc['digest'], 'tag': self.tag}}
        value = {**self.value, 'Config': {'Labels': {'org.opencontainers.image.revision': 'current', 'org.opencontainers.image.version': current['version']}}}
        with patch.object(module, 'run', side_effect=['', json.dumps([value])]), patch.object(module, 'archive_identity') as proof:
            with self.assertRaisesRegex(AssertionError, 'loaded core archive exact image and source'):
                module.load_image(self.path.parent, current)
            proof.assert_not_called()
        with self.assertRaises(RuntimeError):
            with patch.object(module, 'run', side_effect=['', json.dumps([value])]):
                module.load_image(self.path.parent, current, proof={})


class DeliveryScope(unittest.TestCase):
    def test_unregistered_current_and_custom_ids_remain_exact(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location('delivery_identity_test', Path(__file__).parents[1] / 'test-release-delivery.py')
        module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        old = {'source_commit': 'old', 'core_image': {'id': 'old-config'}}
        current = {'source_commit': 'current', 'core_image': {'id': 'current-exact'}}
        module.VERIFIED_RUNTIME_IDS[('old', 'core_image', 'old-config')] = 'proven-runtime'
        self.assertEqual(module.runtime_id(old, 'core_image'), 'proven-runtime')
        self.assertEqual(module.runtime_id(current, 'core_image'), 'current-exact')
        self.assertEqual(module.runtime_id({'source_commit': 'custom', 'core_image': {'id': 'custom-exact'}}, 'core_image'), 'custom-exact')
        self.assertEqual(old['core_image']['id'], 'old-config')

    def test_same_source_different_build_cannot_inherit_baseline_identity(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location('delivery_same_source_test', Path(__file__).parents[1] / 'test-release-delivery.py')
        module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        module.VERIFIED_RUNTIME_IDS[('same-source', 'core_image', 'old-config')] = 'proven-old-runtime'
        for kind in ('core_image', 'caddy_image'):
            manifest = {'source_commit': 'same-source', kind: {'id': 'other-build-id'}}
            self.assertEqual(module.runtime_id(manifest, kind), 'other-build-id')
