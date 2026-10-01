# ===========================================================================
#  Generate a random MySQL password and write it to -OutFile.
#  ---------------------------------------------------------------------------
#  Usage:
#    powershell -NoProfile -ExecutionPolicy Bypass -File _newpass.ps1 -OutFile <path>
#    success -> file contains 24 chars, prints "OK"
#    failure -> no file is produced
#
#  THIS FILE MUST STAY PURE ASCII. DO NOT ADD NON-ASCII TEXT.
#    Windows PowerShell 5.1 reads a .ps1 WITHOUT a BOM using the current ANSI
#    code page (GBK on a Chinese Windows). A UTF-8 Chinese comment therefore
#    gets decoded as GBK garbage, and the garbage can swallow the next code
#    line -- the script then exits 0 while producing an EMPTY password.
#    That is exactly what happened on 2026-10-01: exit code 0, "OK" printed,
#    0-byte output file. Keep comments in English here; put the Chinese
#    explanation in the calling .bat instead (bat files are GBK+C R L F and
#    that combination is known-good on this machine).
#
#  Why a file instead of capturing stdout:
#    When powershell.exe fails to start (policy blocked, bad path, wrong
#    version) it prints a MULTI-LINE BANNER to stdout. A caller doing
#    `for /f ... in ('powershell ...')` will happily set those banner lines
#    into the variable -- so the "random password" becomes an advert for a
#    PowerShell update. Writing a file makes "no file" an unambiguous failure.
#
#  Why a separate .ps1 instead of a one-liner inside the .bat:
#    Inside a .bat, `for /f ... in ('powershell -Command "...|..."')` needs the
#    pipe escaped as ^| , the % doubled as %% , and it still has to dodge the
#    unquoted-paren trap in parenthesised blocks. Three traps stacked. As a
#    standalone file the command line contains nothing that needs escaping.
#
#  Charset: letters + digits only, with 0/O/1/l/I removed (easy to confuse).
#    - letters+digits only -> the password survives cmd metacharacters
#      (& ^ % " < > |) without quoting games
#    - 56 symbols ^ 24 chars ~= 2^139, brute force is not a concern
#  Uses RandomNumberGenerator (a CSPRNG) rather than Get-Random, which is a
#  predictable PRNG and unfit for secrets.
# ===========================================================================

param(
    [Parameter(Mandatory = $true)][string]$OutFile
)

$ErrorActionPreference = 'Stop'

try {
    $chars = 'abcdefghijkmnpqrstuvwxyz' + 'ABCDEFGHJKLMNPQRSTUVWXYZ' + '23456789'
    $bytes = New-Object byte[] 24
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)

    $sb = New-Object System.Text.StringBuilder
    foreach ($b in $bytes) {
        # $b is 0..255; modulo 56 introduces a 4.57x truncation bias, which is
        # irrelevant at ~139 bits of entropy and avoids a rejection-sampling loop.
        [void]$sb.Append($chars[$b % $chars.Length])
    }

    # No BOM, no trailing newline: readers then never have to worry about a
    # BOM or CR/LF becoming part of the password. (The Go side tolerates both,
    # but not giving it the chance is better.)
    $enc = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($OutFile, $sb.ToString(), $enc)
    Write-Output 'OK'
} catch {
    Write-Output ('ERR: ' + $_.Exception.Message)
    exit 1
}
