package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "1fd9e5fe43c06f957b1cb2b25004e7e7db7da9a496e7a6a4bc0940ff245ffb55"
