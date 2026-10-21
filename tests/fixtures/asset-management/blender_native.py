"""Blender-only synthetic scene/motion builder and relocated native verification.

Run with --background --factory-startup --python-exit-code 1 --python FILE --
generate ROOT or verify ROOT. Never open any user project or startup file.
"""

import json
from pathlib import Path
import sys

import bpy
from mathutils import Vector
import numpy as np


def reset():
    bpy.ops.wm.read_factory_settings(use_empty=True)


def build_scene(root, version):
    reset()
    library = root / "lib/prop-library.blend"
    library.parent.mkdir(parents=True, exist_ok=True)
    bpy.ops.mesh.primitive_cube_add(size=0.5, location=(1.5, 0, 0))
    prop = bpy.context.object
    prop.name = "SyntheticLinkedProp"
    bpy.data.libraries.write(str(library), {prop})
    reset()
    bpy.ops.mesh.primitive_cube_add(size=1, location=(0, 0, version / 10))
    cube = bpy.context.object
    cube.name = "SyntheticHero"
    cube["fixture_version"] = version
    material = bpy.data.materials.new("SyntheticChecker")
    material.use_nodes = True
    image = bpy.data.images.load(str(root / "textures/checker.png"))
    image.filepath = "//textures/checker.png"
    texture = material.node_tree.nodes.new("ShaderNodeTexImage")
    texture.image = image
    material.node_tree.links.new(texture.outputs["Color"], material.node_tree.nodes.get("Principled BSDF").inputs["Base Color"])
    cube.data.materials.append(material)
    with bpy.data.libraries.load(str(library), link=True) as (available, linked):
        linked.objects = [name for name in available.objects if name == "SyntheticLinkedProp"]
    for obj in linked.objects:
        bpy.context.collection.objects.link(obj)
    for item in bpy.data.libraries:
        item.filepath = "//lib/prop-library.blend"
    bpy.ops.object.camera_add(location=(4, -6, 4))
    camera = bpy.context.object
    camera.rotation_euler = (Vector((0.5, 0, 0)) - camera.location).to_track_quat("-Z", "Y").to_euler()
    bpy.context.scene.camera = camera
    bpy.ops.object.light_add(type="AREA", location=(0, -3, 5))
    bpy.context.object.data.energy = 300
    scene = bpy.context.scene
    scene.render.engine = "CYCLES"
    scene.cycles.device = "CPU"
    scene.cycles.samples = 1
    scene.render.resolution_x = 64
    scene.render.resolution_y = 64
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = "PNG"
    scene.render.filepath = "//preview.png"
    bpy.ops.wm.save_as_mainfile(filepath=str(root / "scene.blend"), check_existing=False)


def build_motion(root, version):
    reset()
    bpy.ops.object.armature_add(enter_editmode=True)
    rig = bpy.context.object
    rig.name = "SyntheticRig"
    rig.data.edit_bones[0].name = "Root"
    bpy.ops.object.mode_set(mode="POSE")
    bone = rig.pose.bones["Root"]
    for frame, x in [(1, 0), (6, version), (12, 2 * version)]:
        bone.location = (x, 0, 0)
        bone.keyframe_insert(data_path="location", frame=frame)
    bpy.ops.object.mode_set(mode="OBJECT")
    bpy.context.scene.frame_start = 1
    bpy.context.scene.frame_end = 12
    bpy.context.scene.render.fps = 24
    assert rig.animation_data and rig.animation_data.action
    # With neither NLA strips nor all-actions requested, Blender exports the
    # active scene timeline. Leaving use_nla_strips=True would silently produce
    # an unanimated FBX because this minimal fixture deliberately has no strips.
    bpy.ops.export_scene.fbx(filepath=str(root / "motion.fbx"), use_selection=True, add_leaf_bones=False, bake_anim=True, bake_anim_use_nla_strips=False, bake_anim_use_all_actions=False)


def check_relative(path, root):
    assert path.startswith("//"), f"unexpected absolute dependency: {path}"
    resolved = Path(bpy.path.abspath(path)).resolve()
    assert resolved.is_relative_to(root.resolve()), f"dependency escaped relocated asset: {path}"
    assert resolved.is_file(), f"missing dependency: {path}"
    return resolved.relative_to(root).as_posix()


def verify_scene(root, version):
    bpy.ops.wm.open_mainfile(filepath=str(root / "scene.blend"))
    assert bpy.data.objects["SyntheticHero"]["fixture_version"] == version
    libraries = [check_relative(library.filepath, root) for library in bpy.data.libraries]
    assert "lib/prop-library.blend" in libraries
    assert "SyntheticLinkedProp" in bpy.context.scene.objects
    textures = []
    for image in bpy.data.images:
        if image.source == "FILE" and image.filepath:
            textures.append(check_relative(image.filepath, root))
            image.reload()
            assert len(image.pixels) > 0
    assert "textures/checker.png" in textures
    bpy.context.scene.render.filepath = "//continued-preview.png"
    bpy.ops.render.render(write_still=True)
    assert (root / "continued-preview.png").stat().st_size > 0
    bpy.data.objects["SyntheticHero"].location.x += 0.25
    bpy.ops.wm.save_as_mainfile(filepath=str(root / "continued.blend"), check_existing=False)
    bpy.ops.wm.open_mainfile(filepath=str(root / "continued.blend"))
    assert abs(bpy.data.objects["SyntheticHero"].location.x - 0.25) < 0.0001
    assert bpy.context.scene.render.filepath == "//continued-preview.png"
    return {"version": version, "linked_libraries": libraries, "textures": textures, "rendered": True, "continued_scene_saved": True, "continued_scene_reopened": True, "location_x": bpy.data.objects["SyntheticHero"].location.x}


def verify_model(root):
    reset()
    bpy.ops.import_scene.gltf(filepath=str(root / "model.glb"))
    assert any(obj.type == "MESH" for obj in bpy.context.scene.objects)
    reset()
    bpy.ops.wm.obj_import(filepath=str(root / "model.obj"))
    assert any(obj.type == "MESH" for obj in bpy.context.scene.objects)
    images = [image for image in bpy.data.images if image.source == "FILE"]
    assert images, "OBJ material did not resolve external checker texture"
    for image in images:
        assert Path(bpy.path.abspath(image.filepath)).resolve().is_relative_to(root.resolve())
        image.reload()
        assert len(image.pixels) > 0
    hdr = bpy.data.images.load(str(root / "environment.hdr"))
    assert tuple(hdr.size) == (2, 2)
    return {"glb_loaded": True, "obj_material_loaded": True, "hdr_loaded": True}


def verify_motion(root, version):
    reset()
    # The exported scene preserves frames 1..12; Blender's import default adds
    # one frame. Use the explicit metadata-compatible offset instead.
    bpy.ops.import_scene.fbx(filepath=str(root / "motion.fbx"), anim_offset=0.0)
    rigs = [obj for obj in bpy.context.scene.objects if obj.type == "ARMATURE"]
    assert rigs and "Root" in rigs[0].data.bones
    assert bpy.data.actions, "FBX animation was not imported"
    bpy.context.scene.frame_set(1)
    start = rigs[0].pose.bones["Root"].matrix.translation.copy()
    bpy.context.scene.frame_set(12)
    finish = rigs[0].pose.bones["Root"].matrix.translation.copy()
    displacement = (finish - start).length
    assert abs(displacement - 2 * version) < 0.01, f"imported motion displacement differs: {displacement}"
    with np.load(str(root / "trajectory.npz"), allow_pickle=False) as arrays:
        np.testing.assert_array_equal(arrays["positions"], np.array([[0, 0, 0], [version, 0, 0], [2 * version, 0, 0]], dtype=np.float32))
        np.testing.assert_array_equal(arrays["frames"], np.array([1, 6, 12], dtype=np.float32))
    return {"fbx_imported": True, "root_bone": "Root", "animated": True, "displacement": displacement, "npz_arrays_match": True}


def main():
    args = sys.argv[sys.argv.index("--") + 1:]
    if len(args) != 2 or args[0] not in {"generate", "verify"}:
        raise ValueError("expected generate ROOT or verify ROOT")
    command, directory = args
    root = Path(directory).resolve()
    reports = []
    for version in (1, 2):
        scene = root / f"assets/scene/v{version}"
        motion = root / f"assets/motion/v{version}"
        if command == "generate":
            build_scene(scene, version)
            build_motion(motion, version)
        else:
            reports.append({"scene": verify_scene(scene, version), "model": verify_model(root / f"assets/model/v{version}"), "motion": verify_motion(motion, version)})
    if command == "verify":
        (root / "native-verification.json").write_text(json.dumps({"blender": bpy.app.version_string, "source": "synthetic-only", "versions": reports}, indent=2) + "\n")


if __name__ == "__main__":
    main()
