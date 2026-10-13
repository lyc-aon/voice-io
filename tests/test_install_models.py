"""Model paths and installs are testable without downloading model weights."""

import hashlib
import io
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from voice_io import __main__ as cli
from voice_io import install_models, model_store
from voice_io.install_models import MEETING_MODELS, MODELS, Model, ModelInstallError


class InstallerTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.tmp = Path(temp.name)
        self.repo = self.tmp / "repo"
        self.user = self.tmp / "user"
        self.real_model_root = model_store.model_root
        self.repo.mkdir()
        self.user.mkdir()
        repo = patch.object(model_store, "_REPO_MODELS", self.repo)
        user = patch.object(model_store, "model_root", return_value=self.user)
        repo.start()
        user.start()
        self.addCleanup(repo.stop)
        self.addCleanup(user.stop)
        self.vad = Model("vad", "silero_vad_v5.onnx", ("silero_vad_v5.onnx",),
                         "vendor/vad", "abc123")
        self.final = Model("final_en", "final", ("encoder.onnx", "tokens.txt"),
                           "vendor/final", "def456")
        models = patch.object(install_models, "MODELS", (self.vad, self.final))
        meeting = patch.object(install_models, "MEETING_MODELS", ())
        meeting.start()
        self.addCleanup(meeting.stop)
        models.start()
        self.addCleanup(models.stop)

    def test_user_data_path_platforms_and_xdg(self):
        with patch.object(model_store.sys, "platform", "darwin"), \
                patch.object(Path, "home", return_value=self.tmp):
            self.assertEqual(self.real_model_root(),
                             self.tmp / "Library/Application Support/voice-io/models")
        with patch.object(model_store.sys, "platform", "linux"), \
                patch.dict("os.environ", {"XDG_DATA_HOME": str(self.tmp / "xdg")}):
            self.assertEqual(self.real_model_root(), self.tmp / "xdg/voice-io/models")

    def test_resolve_uses_complete_repo_else_installed_and_rejects_traversal(self):
        local = self.repo / "final"
        local.mkdir()
        (local / "encoder.onnx").write_bytes(b"ok")
        self.assertEqual(model_store.resolve_model_path("models/final"), self.user / "final")
        (local / "tokens.txt").write_bytes(b"ok")
        self.assertEqual(model_store.resolve_model_path("models/final"), local)
        for bad in ("../models/final", "models/../../secret", "/tmp/model", "models", "models\\final"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                model_store.resolve_model_path(bad)

    def test_verify_reports_all_missing_files_including_dictionary(self):
        with self.assertRaisesRegex(ModelInstallError, "encoder.onnx, tokens.txt"):
            install_models.verify_models()
        self.assertIn("en.phone", next(model for model in MODELS
                                        if model.name == "kws").required)

    def test_fake_hf_downloader_is_idempotent_and_offline(self):
        urls = []

        def download(url, destination):
            urls.append(url)
            destination.write_bytes(b"model")

        paths = install_models.install_models(downloader=download)
        self.assertEqual(set(paths), {"vad", "final_en"})
        self.assertEqual(len(urls), 3)
        self.assertIn("vendor/final/resolve/def456/tokens.txt", urls[-1])
        install_models.install_models(downloader=lambda *_: self.fail("redownloaded"))
        self.assertEqual(paths, install_models.verify_models())

    def test_failure_never_publishes_partial_model_or_success(self):
        target = self.user / "final"
        target.mkdir()
        (target / "encoder.onnx").write_bytes(b"original")
        (self.repo / self.vad.relative).write_bytes(b"existing")

        def incomplete(url, destination):
            if url.endswith("encoder.onnx"):
                destination.write_bytes(b"model")

        with self.assertRaisesRegex(ModelInstallError, "tokens.txt"):
            install_models.install_models(downloader=incomplete)
        self.assertEqual((target / "encoder.onnx").read_bytes(), b"original")
        self.assertFalse((target / "tokens.txt").exists())

    def test_archive_hash_and_explicit_file_validation(self):
        model = Model("kws", "kws", ("encoder.onnx", "en.phone"),
                      "vendor/release", "tag", archive=True)
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode="w:bz2") as tar:
            for filename in model.required:
                payload = b"valid"
                member = tarfile.TarInfo("kws/" + filename)
                member.size = len(payload)
                tar.addfile(member, io.BytesIO(payload))
        model = Model(model.name, model.relative, model.required, model.repo,
                      model.revision, hashlib.sha256(data.getvalue()).hexdigest(), True)
        with patch.object(install_models, "MODELS", (model,)):
            def download(url, destination):
                destination.write_bytes(data.getvalue())

            result = install_models.install_models(downloader=download)
            self.assertTrue((result["kws"] / "en.phone").is_file())
            (result["kws"] / "en.phone").unlink()
            with self.assertRaisesRegex(ModelInstallError, "en.phone"):
                install_models.verify_models()
            with self.assertRaisesRegex(ModelInstallError, "SHA-256"):
                install_models.install_models(downloader=lambda _url, dest: dest.write_bytes(b"broken"))


    def test_meeting_verify_reports_missing_weights_but_default_does_not(self):
        self.assertEqual(set(install_models.install_models(
            downloader=lambda _url, dest: dest.write_bytes(b"base"))),
            {"vad", "final_en"})
        self.assertEqual(set(install_models.verify_models()), {"vad", "final_en"})
        with patch.object(install_models, "MEETING_MODELS", MEETING_MODELS):
            with self.assertRaises(ModelInstallError):
                install_models.verify_models(meeting=True)

    def test_meeting_fake_downloader_checks_hash_and_only_unpacks_model(self):
        segmentation = Model("segmentation", "sherpa-onnx-pyannote-segmentation-3-0",
                             ("model.int8.onnx",), "k2-fsa/sherpa-onnx",
                             "speaker-segmentation-models", archive=True, release=True)
        embedding = Model("speaker_embedding", "nemo_en_titanet_small.onnx",
                          ("nemo_en_titanet_small.onnx",), "k2-fsa/sherpa-onnx",
                          "speaker-recongition-models", release=True)
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode="w:bz2") as tar:
            for filename in ("model.int8.onnx", "../outside.onnx"):
                payload = b"segmentation"
                member = tarfile.TarInfo(segmentation.relative + "/" + filename)
                member.size = len(payload)
                tar.addfile(member, io.BytesIO(payload))
        segmentation = Model(segmentation.name, segmentation.relative,
                             segmentation.required, segmentation.repo,
                             segmentation.revision,
                             hashlib.sha256(data.getvalue()).hexdigest(),
                             archive=True, release=True)
        embedding = Model(embedding.name, embedding.relative, embedding.required,
                          embedding.repo, embedding.revision,
                          hashlib.sha256(b"speaker").hexdigest(), release=True)
        urls = []

        def download(url, destination):
            urls.append(url)
            destination.write_bytes(data.getvalue() if url.endswith(".tar.bz2") else b"speaker")

        with patch.object(install_models, "MEETING_MODELS", (segmentation, embedding)):
            paths = install_models.install_models(meeting=True, downloader=download)
            self.assertEqual(set(paths), {"vad", "final_en", "segmentation", "speaker_embedding"})
            self.assertEqual((paths["segmentation"] / "model.int8.onnx").read_bytes(),
                             b"segmentation")
            self.assertFalse((paths["segmentation"] / "outside.onnx").exists())
            self.assertEqual(paths["speaker_embedding"].read_bytes(), b"speaker")
            self.assertEqual(urls[-2:], [install_models._url(segmentation),
                                          install_models._url(embedding)])
            self.assertEqual(paths, install_models.verify_models(meeting=True))
            install_models.install_models(meeting=True, downloader=lambda *_: self.fail("redownloaded"))
            paths["speaker_embedding"].unlink()
            with self.assertRaisesRegex(ModelInstallError, "SHA-256"):
                install_models.install_models(
                    meeting=True, downloader=lambda _url, dest: dest.write_bytes(b"wrong"))
            self.assertFalse(paths["speaker_embedding"].exists())

    def test_meeting_model_resolution_prefers_complete_repo_else_user(self):
        model = MEETING_MODELS[0]
        with patch.object(install_models, "MEETING_MODELS", MEETING_MODELS):
            repo = self.repo / model.relative
            user = self.user / model.relative
            repo.mkdir()
            user.mkdir()
            (user / "model.int8.onnx").write_bytes(b"ok")
            (user / "LICENSE").write_bytes(b"MIT")
            path = f"models/{model.relative}"
            self.assertEqual(model_store.resolve_model_path(path), user)
            (repo / "model.int8.onnx").write_bytes(b"ok")
            (repo / "LICENSE").write_bytes(b"MIT")
            self.assertEqual(model_store.resolve_model_path(path), repo)

    def test_models_cli_meeting_flag_passes_through_for_both_actions(self):
        for action in ("install", "verify"):
            for meeting in (False, True):
                with self.subTest(action=action, meeting=meeting), \
                        patch.object(cli.sys, "argv",
                                     ["voice-io", "models", action] + (["--meeting"] if meeting else [])), \
                        patch.object(cli.Config, "load"), \
                        patch.object(install_models, "install_models", return_value={}) as installer, \
                        patch.object(install_models, "verify_models", return_value={}) as verifier:
                    self.assertEqual(cli.main(), 0)
                    (installer if action == "install" else verifier).assert_called_once_with(
                        meeting=meeting)
