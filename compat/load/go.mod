// The traffic generator for the hardening gates. A module of its own, like the
// other helpers under compat/, so that nothing a gate needs in order to send
// hostile bodies is ever compiled into the product's binary.
module trapline.local/compat/load

go 1.26.1

toolchain go1.26.6

require github.com/klauspost/compress v1.19.2
