This directory holds scripts called by `make.sh` in the parent directory.

Each script is named after the bundle it creates.
They should not be called directly - instead, pass it as argument to make.sh, for example:

```
./project/make.sh test
./project/make.sh binary ubuntu

# Or to run all bundles:
./project/make.sh
```

To add a bundle:

* Create a shell-compatible file here
* Add it to $DEFAULT_BUNDLES in make.sh