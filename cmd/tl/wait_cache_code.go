package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "5d21d9965075b0aa56b94a6ac8a1eee6bdc78f7ef29447032bf4dcf8e99ffb89"
