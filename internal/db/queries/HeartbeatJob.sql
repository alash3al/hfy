UPDATE jobs SET lease_until = now() + @lease::interval WHERE id = @id AND state = 'running'
