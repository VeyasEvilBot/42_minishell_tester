# Extra automatable mandatory cases recovered from Vienna evaluation feedback.
# Interactive signal/readline/heredoc-interrupt cases remain manual by design.

unset PATH
/bin/echo absolute-path-ok

export PATH=""
/bin/echo empty-path-did-not-hang

export PATH=:
/bin/echo colon-path-did-not-hang

export PATH=::/bin
printf duplicate-empty-path-ok

export A=hello
export A
echo "$A"

export A=1
export A+=2
echo "$A"

export hi hi=2 hi=8 hi#=sdf
echo "$hi"

unset HOME
cd
echo $?

unset OLDPWD
cd -
echo $?

unset PWD
cd /tmp
pwd

cd /definitely-not-a-directory
echo $?

pwd
export PWD=/definitely/fake
pwd

echo -n -n hello

echo -nnnn hello

echo -sdjkfhsd

echo $HOME$USER

echo $"HOME"$USER

""
echo $?

< definitely_does_not_exist
echo $?

cat Makefile > > > > out

nosuch1 | nosuch2

echo foo | < definitely_does_not_exist

export > out
/bin/test -f out
echo $?

cat << 'EOF'
$USER
EOF

cat << "EOF"
$USER
EOF

cat << EOF
$USER
EOF

exit 0

exit 255

exit 256

exit -1

exit abc

exit 123 abc
echo survived

exit abc 123

exit 999999999999999999999999999999

VAR=123 definitely_not_a_command

printf 'one\ntwo\n' | cat | cat

echo quoted '|' pipe

echo quoted '>' redirect

export A="   A   B   "
$A

unset DOES_NOT_EXIST
echo $?

echo "$UNSET_VARIABLE"

$UNSET_VARIABLE

env | /usr/bin/sort

export | /usr/bin/sort
