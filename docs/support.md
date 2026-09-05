# What brig runs on

brig boots each coding agent inside its own lightweight virtual machine. That
needs a Mac with Apple Silicon, or a Linux machine.

| Your computer | Does brig run? |
| --- | --- |
| Mac with Apple Silicon (M1 or newer), macOS 15 or later | Yes |
| Mac with Apple Silicon (M1 or newer), macOS 14 | Yes, if you start it with `BRIG_HYPERVISOR=vz` |
| Intel Mac | No, brig needs Apple Silicon |
| Linux, x86-64 or ARM64 | Yes, with `nerdctl` or Docker installed |

On macOS, brig boots the virtual machine itself, so there is nothing else to
install. On Linux it uses nerdctl and containerd, and Docker works in their
place.
