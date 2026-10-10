// Shows why the last sign-in failed (?e=wrong | ?e=locked).
const e = new URLSearchParams(location.search).get('e');
const msg = document.getElementById('msg');
if (e === 'wrong') msg.textContent = 'Wrong password.';
if (e === 'locked') msg.textContent = 'Too many wrong attempts. Try again in 15 minutes.';
