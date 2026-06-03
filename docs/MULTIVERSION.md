# Building the companion mod for another Minecraft version

The `companion/` Fabric mod defaults to Minecraft 1.21.5, but the version
coordinates are no longer hardcoded in source. They live as Gradle properties,
so you can build for a different Minecraft version without editing any files.

## The three properties

Set these in `companion/gradle.properties`, or override them per build with
`-P` flags on the Gradle CLI:

| Property              | Meaning                          | Default            |
| --------------------- | -------------------------------- | ------------------ |
| `minecraft_version`   | Target Minecraft version         | `1.21.5`           |
| `loader_version`      | Fabric Loader version            | `0.16.14`          |
| `fabric_api_version`  | Fabric API build for that MC ver | `0.128.2+1.21.5`   |

These feed both `build.gradle` (dependency coordinates) and
`fabric.mod.json` (the `minecraft` depends entry, expanded at build time by
`processResources`).

## Build for the default (1.21.5)

```sh
cd companion
JAVA_HOME=/opt/homebrew/opt/openjdk@21 ./gradlew build --no-daemon
```

Output: `companion/build/libs/tickwarden-companion-0.2.0.jar`

## Build for a different version

Either edit `companion/gradle.properties`, or pass the values on the CLI:

```sh
cd companion
JAVA_HOME=/opt/homebrew/opt/openjdk@21 ./gradlew build --no-daemon \
  -Pminecraft_version=1.21.4 \
  -Pfabric_api_version=0.119.5+1.21.4 \
  -Ploader_version=0.16.14
```

You usually only need to change `minecraft_version` and
`fabric_api_version` together; the Fabric Loader version moves independently
and is fine to leave alone across nearby Minecraft versions.

## Finding the right fabric-api build

`fabric_api_version` must be a build published *for your exact Minecraft
version*. The suffix after `+` is the Minecraft version (e.g.
`0.119.5+1.21.4`).

- Modrinth: <https://modrinth.com/mod/fabric-api/versions> — filter by your
  Minecraft version and copy the version string (it's the Maven version, the
  part shown as `x.y.z+1.2.3`).
- Fabric versions page: <https://fabricmc.net/develop/> — pick your Minecraft
  version and it lists the recommended Loader and Fabric API versions to use.

## Caveat: APIs can move between major versions

The companion's per-tick / hotspot code uses stable server APIs (TPS/MSPT,
`ChunkMap.getChunks()` via an access widener). These are reasonably stable, but
a major Minecraft version bump can rename, relocate, or remove a method, or
change the access-widener target. If that happens the mod won't silently
misbehave — it will fail to compile.

So after bumping `minecraft_version`, always rebuild and let the compiler catch
breaks:

```sh
cd companion && ./gradlew build --no-daemon
```

If the build fails on a missing/changed symbol, the source under
`companion/src/main/java/...` (and possibly the access widener at
`companion/src/main/resources/tickwarden-companion.accesswidener`) needs a small
update for that Minecraft version.
