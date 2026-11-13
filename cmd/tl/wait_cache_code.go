package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "7c5c2f081dfe43fb9c7a081b0fcb1cfba6e102cf7ddeb103cf8ab3419f885b5c"
