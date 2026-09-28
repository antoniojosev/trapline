// A module of its own, and deliberately not this repository's.
//
// It verifies the webhook signature with an implementation written only from
// docs/alerts/webhooks.md, using nothing but the standard library. If it
// imported trapline's own domain package it would prove that the code agrees
// with itself, which is not the question: the question is whether somebody who
// reads the documentation can write a receiver that works.
module trapline.local/alerts-receiver

go 1.26
