# glov's commands stay hand-written, and glov checks usage before glovd does

On Windows hosts, `glov`'s CLI (`windows/cmd/glov/cli.go`, `main_windows.go`) and its MCP tools (`mcp_windows.go`) each
parse and check their own arguments before calling glovd, which checks them again. We considered one operation
table (method, params, long or short, how to print the result) feeding both, with glovd as the only validator. We
rejected it for two reasons:

- **Usage errors stay local.** The client-side checks are what let `glov` answer a usage error with exit code 2 even
  while glovd is not running, and give the MCP client a plain tool error.
- **No leverage yet.** The table would be a rewrite of the CLI and MCP layers, and no planned commands need it.

Rules shared by both surfaces, such as disk sizes in whole GiB (`parseDiskGiB`) or `api.MaxGuestUpdateTimeout`, live
in one function or constant instead. Revisit this when adding commands becomes the bottleneck.
