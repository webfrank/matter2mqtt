#!/usr/bin/env python3
"""Dump the Matter data model as JSON, for embedding in matter2mqtt.

Run INSIDE the matter-server container so names match the CHIP SDK your
controller actually uses. Note the -i: without it docker does not forward
stdin, python reads nothing, and you get an empty file.

    docker exec -i matter-server python3 - < tools/dump-model.py > model.json

Or copy it in, which avoids the stdin question entirely:

    docker cp tools/dump-model.py matter-server:/tmp/dump-model.py
    docker exec matter-server python3 /tmp/dump-model.py > model.json

Add --debug for a breakdown of what was found, on stderr.

Regenerate after any matter-server upgrade that bumps the SDK version.
"""

import json
import sys

DEBUG = "--debug" in sys.argv


def log(msg):
    print(f"[dump-model] {msg}", file=sys.stderr)


def debug(msg):
    if DEBUG:
        log(msg)


# Device types are not exposed by the SDK python bindings in every build, so
# this table is the floor: output is never worse than the shipped seed.
FALLBACK_DEVICE_TYPES = {
    10: "DoorLock", 15: "GenericSwitch", 17: "PowerSource", 19: "BridgedNode",
    21: "ContactSensor", 22: "RootNode", 256: "OnOffLight", 257: "DimmableLight",
    259: "OnOffLightSwitch", 262: "LightSensor", 263: "OccupancySensor",
    266: "OnOffPlugInUnit", 267: "DimmablePlugInUnit", 268: "ColorTemperatureLight",
    269: "ExtendedColorLight", 514: "WindowCovering", 769: "Thermostat",
    770: "TemperatureSensor", 773: "PressureSensor", 774: "FlowSensor",
    775: "HumiditySensor",
}


def import_objects():
    try:
        from chip.clusters import Objects
        return Objects
    except ImportError as exc:
        log(f"cannot import chip.clusters.Objects: {exc}")
        log("Are you running this inside the matter-server container?")
        log("Try: docker exec -i matter-server python3 - < tools/dump-model.py > model.json")
        sys.exit(1)


def safe_int(value):
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def cluster_name_from(attr_cls, fallback_id):
    """Attribute classes are nested as <Cluster>.Attributes.<Attribute>, so the
    qualified name carries the cluster name even without a cluster registry."""
    qual = getattr(attr_cls, "__qualname__", "")
    head = qual.split(".")[0]
    if head and head != "Attributes":
        return head
    return f"Cluster{fallback_id}"


def via_registry(objects):
    """Preferred: the SDK's own ALL_ATTRIBUTES registry, keyed by numeric id."""
    registry = getattr(objects, "ALL_ATTRIBUTES", None)
    if not registry:
        debug("ALL_ATTRIBUTES not present")
        return {}

    all_clusters = getattr(objects, "ALL_CLUSTERS", {}) or {}
    out = {}
    for cluster_id, attrs in registry.items():
        cid = safe_int(cluster_id)
        if cid is None:
            continue

        name = None
        cluster_cls = all_clusters.get(cluster_id)
        if cluster_cls is not None:
            name = getattr(cluster_cls, "__name__", None)

        attributes = {}
        for attr_id, attr_cls in (attrs or {}).items():
            aid = safe_int(attr_id)
            if aid is None:
                continue
            attributes[str(aid)] = getattr(attr_cls, "__name__", f"Attribute{aid}")
            if name is None:
                name = cluster_name_from(attr_cls, cid)

        out[str(cid)] = {"name": name or f"Cluster{cid}", "attributes": attributes}

    debug(f"registry strategy: {len(out)} clusters")
    return out


def via_introspection(objects):
    """Fallback: walk the module looking for Cluster subclasses."""
    try:
        from chip.clusters.ClusterObjects import Cluster, ClusterAttributeDescriptor
    except ImportError as exc:
        debug(f"introspection unavailable: {exc}")
        return {}

    out = {}
    for name in dir(objects):
        cluster = getattr(objects, name, None)
        if not isinstance(cluster, type) or not issubclass(cluster, Cluster):
            continue
        if cluster is Cluster:
            continue

        cid = safe_int(getattr(cluster, "id", None))
        if cid is None:
            continue

        attributes = {}
        attr_ns = getattr(cluster, "Attributes", None)
        if attr_ns is not None:
            for attr_name in dir(attr_ns):
                attr = getattr(attr_ns, attr_name, None)
                if not isinstance(attr, type) or not issubclass(attr, ClusterAttributeDescriptor):
                    continue
                aid = safe_int(getattr(attr, "attribute_id", None))
                if aid is not None:
                    attributes[str(aid)] = attr_name

        out[str(cid)] = {"name": name, "attributes": attributes}

    debug(f"introspection strategy: {len(out)} clusters")
    return out


def collect_commands(objects, clusters):
    """Command names are a nicety; failure here must not lose the model."""
    try:
        from chip.clusters.ClusterObjects import Cluster
    except ImportError:
        return

    by_id = {}
    for name in dir(objects):
        cluster = getattr(objects, name, None)
        if isinstance(cluster, type) and issubclass(cluster, Cluster):
            cid = safe_int(getattr(cluster, "id", None))
            if cid is not None:
                by_id[str(cid)] = cluster

    for cid, entry in clusters.items():
        cluster = by_id.get(cid)
        cmd_ns = getattr(cluster, "Commands", None) if cluster else None
        if cmd_ns is not None:
            entry["commands"] = sorted(c for c in dir(cmd_ns) if not c.startswith("_"))


def collect_device_types():
    for module_path, attr in (
        ("chip.testing.matter_testing", "device_type_id_to_name"),
        ("chip.clusters.Objects", "ALL_DEVICE_TYPES"),
    ):
        try:
            module = __import__(module_path, fromlist=[attr])
            table = getattr(module, attr, None)
            if table:
                out = {}
                for k, v in table.items():
                    kid = safe_int(k)
                    if kid is not None:
                        out[str(kid)] = v if isinstance(v, str) else getattr(v, "__name__", str(v))
                if out:
                    debug(f"device types from {module_path}.{attr}: {len(out)}")
                    return out
        except Exception as exc:  # noqa: BLE001 - any failure just means "try the next source"
            debug(f"{module_path}.{attr} unavailable: {exc}")

    debug("using the built-in device type table")
    return {str(k): v for k, v in FALLBACK_DEVICE_TYPES.items()}


def main():
    objects = import_objects()
    log(f"chip.clusters.Objects loaded from {getattr(objects, '__file__', 'unknown')}")

    clusters = via_registry(objects)
    if not clusters:
        log("attribute registry empty, falling back to module introspection")
        clusters = via_introspection(objects)

    if not clusters:
        log("FAILED: no clusters discovered by either strategy.")
        log("Re-run with --debug and report the SDK version so this can be fixed:")
        log("  docker exec -i matter-server python3 - --debug < tools/dump-model.py > model.json")
        sys.exit(1)

    collect_commands(objects, clusters)
    device_types = collect_device_types()

    named = sum(1 for c in clusters.values() if not c["name"].startswith("Cluster"))
    attr_total = sum(len(c["attributes"]) for c in clusters.values())

    json.dump({"clusters": clusters, "device_types": device_types},
              sys.stdout, indent=1, sort_keys=True)
    sys.stdout.write("\n")

    log(f"wrote {len(clusters)} clusters ({named} named), "
        f"{attr_total} attributes, {len(device_types)} device types")
    if named < len(clusters):
        log(f"note: {len(clusters) - named} clusters had no resolvable name and are "
            f"labelled ClusterNNN - they still work, just without a friendly name")


if __name__ == "__main__":
    main()
