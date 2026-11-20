package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "28a6d383807903c1b9bdd86f5a2ef3ea0259f0a3b4c05067cabbcc8f1f10e6d2"
