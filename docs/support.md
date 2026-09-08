# What brig runs on

brig boots each coding agent inside its own lightweight virtual machine. That
needs a Mac with Apple Silicon, or a Linux machine.

| Your computer | Does brig run? |
| --- | --- |
| Mac with Apple Silicon (M1 or newer), macOS 15 or later | Yes |
| Mac with Apple Silicon (M1 or newer), macOS 14 | Yes, if you start it with `BRIG_HYPERVISOR=vz` |
| Intel Mac | No, brig needs Apple Silicon |
| Linux, x86-64 or ARM64 | Yes, with `nerdctl` and containerd installed |

On macOS the virtual machine is booted by [hull](https://github.com/brig-sh/hull),
which the Homebrew cask installs with brig; a from-source brig needs hull on
`PATH`. On Linux it uses nerdctl and containerd with the urunc shim. Docker is
accepted in nerdctl's place for an image that carries its own kernel; a profile
marked `genericBoot`, which is six of the eight shipped ones, is refused on
Docker because it does not pass the boot annotations through to the runtime.
