# glov

Canonical product terminology. Existing interfaces may retain older terminology until the planned rename is implemented.

## Language

**glov**:
The product for AI-driven virtual machine workflows.

**AI agent**:
An external AI-driven actor that uses glov to operate environments.
_Avoid_: Guest agent

**Guest Service**:
The resident glov service inside a guest that carries out requests from the host.
_Avoid_: Agent, guest agent

**VM engine**:
The hypervisor backend that runs a VM: QEMU or Virtualization.framework on macOS, the Host Compute Service on Windows.
_Avoid_: Engine, hypervisor
