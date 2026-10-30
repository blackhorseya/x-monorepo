<!-- The inventory 0009 rollback as docs/operations.md told operators at d2f4035 (PR #318 first head); kept so the test can run that real entry. -->
Rollback：`task migrate -- down` 一步（`DROP CONSTRAINT`）；版本是 dirty 時 down 同樣被拒，先做第 3 步。
