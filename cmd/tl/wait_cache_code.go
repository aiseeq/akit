package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "db9b256f07297c9d20c7ef45b99f14cd29034f530ec0229d1f0fb4e9d05de7d9"
