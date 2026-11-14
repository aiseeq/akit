package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "16ad0b8a59dfd75eaff52e0a1b6715bc75eb65ef41a4bcae06d61a7d674d17c2"
