package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "1bb9a67f44d4a99a9f9469181d56af29a3a1a66c3f8b1176b84ff3cdf0c624a4"
