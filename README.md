# sparsemapper

Turn an Apple `sparsebundle` into a single block device using `loop` and `devicemapper`.

It is cursed.

It has been tested with up to 25,000 sparse bands in a sparsebundle totaling 8TiB in size.

## Usage

```
  sparsemapper [options] sparsebundle

Application Options:
  -v          verbose (more is more)
  -d, --name= name of device-mapper device to create; if not specified, one will be generated
  -N          pretend

Help Options:
  -h, --help  Show this help message
```
