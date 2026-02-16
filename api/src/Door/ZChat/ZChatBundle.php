<?php

namespace App\Door\ZChat;

use Symfony\Component\HttpKernel\Bundle\Bundle;

class ZChatBundle extends Bundle
{
    public function getPath(): string
    {
        return \dirname(__DIR__ . '/ZChat');
    }
}
