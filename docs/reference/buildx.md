# buildx

```
docker buildx [OPTIONS] COMMAND
```

<!---MARKER_GEN_START-->
Extended build capabilities with BuildKit

### Subcommands

| Name                                                                         | Description                                |
|:-----------------------------------------------------------------------------|:-------------------------------------------|
| [`_INTERNAL_DAP_ATTACH_CONTAINER`](buildx__INTERNAL_DAP_ATTACH_CONTAINER.md) |                                            |
| [`_INTERNAL_SERVE`](buildx__INTERNAL_SERVE.md)                               |                                            |
| [`bake`](buildx_bake.md)                                                     | Build from a file                          |
| [`build`](buildx_build.md)                                                   | Start a build                              |
| [`create`](buildx_create.md)                                                 | Create a new builder instance              |
| [`dap`](buildx_dap.md)                                                       |                                            |
| [`debug-shell`](buildx_debug-shell.md)                                       | Start a monitor                            |
| [`du`](buildx_du.md)                                                         | Disk usage                                 |
| [`imagetools`](buildx_imagetools.md)                                         | Commands to work on images in registry     |
| [`inspect`](buildx_inspect.md)                                               | Inspect current builder instance           |
| [`install`](buildx_install.md)                                               | Install buildx as a 'docker builder' alias |
| [`ls`](buildx_ls.md)                                                         | List builder instances                     |
| [`prune`](buildx_prune.md)                                                   | Remove build cache                         |
| [`rm`](buildx_rm.md)                                                         | Remove a builder instance                  |
| [`stop`](buildx_stop.md)                                                     | Stop builder instance                      |
| [`uninstall`](buildx_uninstall.md)                                           | Uninstall the 'docker builder' alias       |
| [`use`](buildx_use.md)                                                       | Set the current builder instance           |
| [`version`](buildx_version.md)                                               | Show buildx version information            |


### Options

| Name                    | Type     | Default | Description                              |
|:------------------------|:---------|:--------|:-----------------------------------------|
| [`--builder`](#builder) | `string` |         | Override the configured builder instance |


<!---MARKER_GEN_END-->

## Examples

### <a name="builder"></a> Override the configured builder instance (--builder)

You can also use the `BUILDX_BUILDER` environment variable.
